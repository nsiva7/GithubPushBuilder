package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nsiva7/GithubPushBuilder/internal/config"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

const repoScript = ".our_container_dir/our_container_script.sh"

type fixture struct {
	t      *testing.T
	eng    *Engine
	st     *store.Store
	repo   store.Repo
	src    string // the "remote" git repository
	data   string
	cancel context.CancelFunc
}

// newFixture starts an engine on a temp data dir, with one registered repo
// whose clone URL is a local git repository containing the given build script.
func newFixture(t *testing.T, script string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	killGrace = 500 * time.Millisecond
	f := &fixture{t: t, src: t.TempDir(), data: t.TempDir()}

	f.git("init", "--quiet", "-b", "main")
	if script != "" {
		f.write(filepath.Join(f.src, repoScript), script)
	}
	f.write(filepath.Join(f.src, "README"), "hello\n")
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", "Initial commit")

	st, err := store.Open(filepath.Join(f.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	id, err := st.CreateRepo(store.Repo{FullName: "acme/app", CloneURL: f.src, WebhookSecret: "hook-secret-value", Enabled: true}, []string{"main"})
	if err != nil {
		t.Fatal(err)
	}
	f.repo, _ = st.GetRepo(id)

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.eng = New(config.Config{DataDir: f.data, RepoScript: repoScript}, st)
	if err := f.eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		f.eng.Wait()
		st.Close()
	})
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.src
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dana", "GIT_AUTHOR_EMAIL=dana@example.com",
		"GIT_COMMITTER_NAME=Dana", "GIT_COMMITTER_EMAIL=dana@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) run(nb store.NewBuild) int64 {
	f.t.Helper()
	nb.RepoID, nb.Branch = f.repo.ID, "main"
	if nb.Trigger == "" {
		nb.Trigger = store.TriggerManual
	}
	id, err := f.eng.Enqueue(nb)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) waitFor(id int64, want func(store.Build) bool) store.Build {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := f.st.GetBuild(id)
		if err != nil {
			f.t.Fatal(err)
		}
		if want(b) {
			return b
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("build #%d stuck in %s\n%s", id, b.Status, f.log(id))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fixture) finished(id int64) store.Build {
	return f.waitFor(id, func(b store.Build) bool { return !b.Active() })
}

func (f *fixture) log(id int64) string {
	data, _ := os.ReadFile(f.eng.LogPath(id))
	return string(data)
}

func TestSuccessfulBuild(t *testing.T) {
	f := newFixture(t, `#!/usr/bin/env bash
set -euo pipefail
echo "repo=$REPO branch=$BRANCH sha=$COMMIT_SHA id=$BUILD_ID"
echo "from pre: $FROM_PRE"
echo "the secret is $API_KEY"
test "$PWD" = "$WORKSPACE"
mkdir -p "$ARTIFACTS_DIR/sub"
printf 'app' > "$ARTIFACTS_DIR/app.apk"
printf 'map' > "$ARTIFACTS_DIR/sub/mapping.txt"
`)
	f.write(filepath.Join(f.data, "scripts", "pre.sh"), "echo FROM_PRE=handed-over >> \"$BUILD_ENV\"\n")
	f.write(filepath.Join(f.data, "scripts", "post.sh"), "echo \"post saw $BUILD_STATUS\"\n")
	f.st.CreateSecret("API_KEY", "s3cr3t-value", true, nil)
	sha := f.git("rev-parse", "HEAD")

	id := f.run(store.NewBuild{}) // no SHA: build the branch head
	b := f.finished(id)
	log := f.log(id)
	if b.Status != store.StatusSuccess {
		t.Fatalf("status %s (%s)\n%s", b.Status, b.Message, log)
	}
	if b.CommitSHA != sha || b.CommitMessage != "Initial commit" || b.Author != "Dana" {
		t.Errorf("commit not resolved: %+v", b)
	}
	for _, want := range []string{"repo=acme/app branch=main sha=" + sha, "from pre: handed-over", "the secret is ***", "post saw success"} {
		if !strings.Contains(log, want) {
			t.Errorf("log is missing %q\n%s", want, log)
		}
	}
	if strings.Contains(log, "s3cr3t-value") {
		t.Error("the secret value leaked into the log")
	}

	arts, _ := f.st.ListArtifacts(id)
	if len(arts) != 2 || arts[0].Path != "app.apk" || arts[1].Path != "sub/mapping.txt" {
		t.Fatalf("artifacts: %+v", arts)
	}
	sum := sha256.Sum256([]byte("app"))
	if arts[0].SHA256 != hex.EncodeToString(sum[:]) || arts[0].Size != 3 {
		t.Errorf("wrong checksum or size: %+v", arts[0])
	}
	if _, err := os.Stat(filepath.Join(f.eng.ArtifactDir("acme/app", id), "app.apk")); err != nil {
		t.Errorf("artifact file missing: %v", err)
	}
	if _, err := os.Stat(f.eng.workspaceDir(id)); !os.IsNotExist(err) {
		t.Error("the workspace was not cleaned up")
	}
}

func TestBuildsThePushedCommitNotTheHead(t *testing.T) {
	f := newFixture(t, "cat README\n")
	old := f.git("rev-parse", "HEAD")
	f.write(filepath.Join(f.src, "README"), "newer\n")
	f.git("commit", "--quiet", "-am", "Second commit")

	id := f.run(store.NewBuild{CommitSHA: old, Trigger: store.TriggerWebhook})
	b := f.finished(id)
	if b.Status != store.StatusSuccess || !strings.Contains(f.log(id), "hello") || strings.Contains(f.log(id), "newer") {
		t.Errorf("status %s\n%s", b.Status, f.log(id))
	}
}

func TestMissingRepoScriptFails(t *testing.T) {
	f := newFixture(t, "")
	f.write(filepath.Join(f.data, "scripts", "post.sh"), "echo \"post saw $BUILD_STATUS\"\n")
	id := f.run(store.NewBuild{})
	b := f.finished(id)
	if b.Status != store.StatusFailed || !strings.Contains(b.Message, repoScript+" is missing") {
		t.Errorf("status %s, message %q", b.Status, b.Message)
	}
	if !strings.Contains(f.log(id), "post saw failed") {
		t.Errorf("the post script did not run\n%s", f.log(id))
	}
}

func TestPreScriptFailureSkipsRepoScript(t *testing.T) {
	f := newFixture(t, "echo REPO-SCRIPT-RAN\n")
	f.write(filepath.Join(f.data, "scripts", "pre.sh"), "echo pre is broken; exit 3\n")
	f.write(filepath.Join(f.data, "scripts", "post.sh"), "echo \"post saw $BUILD_STATUS\"\n")
	id := f.run(store.NewBuild{})
	b := f.finished(id)
	log := f.log(id)
	if b.Status != store.StatusFailed || !strings.Contains(b.Message, "Pre script failed (exit code 3)") {
		t.Errorf("status %s, message %q", b.Status, b.Message)
	}
	if strings.Contains(log, "REPO-SCRIPT-RAN") || !strings.Contains(log, "post saw failed") {
		t.Errorf("unexpected log\n%s", log)
	}
}

func TestPerRepoScriptOverridesGlobal(t *testing.T) {
	f := newFixture(t, "true\n")
	f.write(filepath.Join(f.data, "scripts", "pre.sh"), "echo GLOBAL-PRE\n")
	f.write(filepath.Join(f.data, "scripts", "acme", "app", "pre.sh"), "echo OWN-PRE\n")
	id := f.run(store.NewBuild{})
	f.finished(id)
	if log := f.log(id); !strings.Contains(log, "OWN-PRE") || strings.Contains(log, "GLOBAL-PRE") {
		t.Errorf("unexpected log\n%s", log)
	}
}

func TestRepoScriptFailure(t *testing.T) {
	f := newFixture(t, "echo compiling; exit 7\n")
	b := f.finished(f.run(store.NewBuild{}))
	if b.Status != store.StatusFailed || b.Message != "Repo script failed: exit code 7" {
		t.Errorf("status %s, message %q", b.Status, b.Message)
	}
}

// A script that leaves a child running in the background, to check that the
// whole process group dies.
const slowScript = `#!/usr/bin/env bash
sleep 300 &
echo "child $!" > "$ARTIFACTS_DIR/child.pid"
echo started
wait
`

func childGone(t *testing.T, f *fixture, id int64) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.eng.ArtifactDir("acme/app", id), "child.pid"))
	if err != nil {
		t.Fatalf("no pid file: %v", err)
	}
	pid := strings.TrimSpace(strings.TrimPrefix(string(data), "child "))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + pid); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("background process %s survived", pid)
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	timeoutUnit = 300 * time.Millisecond // a "1 minute" timeout lasts 300ms
	defer func() { timeoutUnit = time.Minute }()
	f := newFixture(t, slowScript)
	f.write(filepath.Join(f.data, "scripts", "post.sh"), "echo \"post saw $BUILD_STATUS\"\n")
	f.repo.TimeoutMinutes = 1
	f.st.UpdateRepo(f.repo, f.repo.Branches)

	id := f.run(store.NewBuild{})
	b := f.finished(id)
	if b.Status != store.StatusTimedOut {
		t.Fatalf("status %s (%s)\n%s", b.Status, b.Message, f.log(id))
	}
	if !strings.Contains(f.log(id), "post saw timed_out") {
		t.Errorf("the post script did not run\n%s", f.log(id))
	}
	childGone(t, f, id)
}

func TestCancelRunningBuild(t *testing.T) {
	f := newFixture(t, slowScript)
	f.write(filepath.Join(f.data, "scripts", "post.sh"), "echo \"post saw $BUILD_STATUS\"\n")
	id := f.run(store.NewBuild{})
	f.waitFor(id, func(store.Build) bool { return strings.Contains(f.log(id), "started") })
	if err := f.eng.Cancel(id); err != nil {
		t.Fatal(err)
	}
	b := f.finished(id)
	if b.Status != store.StatusCancelled || !strings.Contains(f.log(id), "post saw cancelled") {
		t.Errorf("status %s\n%s", b.Status, f.log(id))
	}
	childGone(t, f, id)
	if err := f.eng.Cancel(id); err != ErrNotActive {
		t.Errorf("cancelling a finished build: %v", err)
	}
}

func TestQueueSupersedeAndCancelWhileWaiting(t *testing.T) {
	f := newFixture(t, slowScript)
	running := f.run(store.NewBuild{Trigger: store.TriggerWebhook})
	f.waitFor(running, func(b store.Build) bool { return b.Status == store.StatusRunning })

	waiting1 := f.run(store.NewBuild{Trigger: store.TriggerWebhook})
	waiting2 := f.run(store.NewBuild{Trigger: store.TriggerWebhook})
	if b, _ := f.st.GetBuild(waiting1); b.Status != store.StatusSuperseded {
		t.Errorf("older waiting push: %s, want superseded", b.Status)
	}
	if b, _ := f.st.GetBuild(running); b.Status != store.StatusRunning {
		t.Errorf("the running build was touched: %s", b.Status)
	}
	if err := f.eng.Cancel(waiting2); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.st.GetBuild(waiting2); b.Status != store.StatusCancelled {
		t.Errorf("waiting build after cancel: %s", b.Status)
	}
	f.eng.Cancel(running)
	f.finished(running)
}

func TestShutdownLeavesBuildForRequeue(t *testing.T) {
	f := newFixture(t, slowScript)
	id := f.run(store.NewBuild{})
	f.waitFor(id, func(store.Build) bool { return strings.Contains(f.log(id), "started") })

	f.cancel() // the service stops
	f.eng.Wait()
	if b, _ := f.st.GetBuild(id); b.Status != store.StatusRunning {
		t.Fatalf("after shutdown: %s, want running (so that startup re-queues it)", b.Status)
	}
	childGone(t, f, id)

	// The next start picks it up again, from the beginning.
	ctx, cancel := context.WithCancel(context.Background())
	eng := New(config.Config{DataDir: f.data, RepoScript: repoScript}, f.st)
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); eng.Wait() }()
	f.waitFor(id, func(b store.Build) bool {
		return b.Status == store.StatusRunning && strings.Contains(f.log(id), "started")
	})
	if err := eng.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if b := f.finished(id); b.Status != store.StatusCancelled {
		t.Errorf("status %s", b.Status)
	}
}

func TestRecoveryQueuesMissedPush(t *testing.T) {
	f := newFixture(t, "echo ok\n")
	first := f.finished(f.run(store.NewBuild{Trigger: store.TriggerWebhook, CommitSHA: f.git("rev-parse", "HEAD")}))
	if first.Status != store.StatusSuccess {
		t.Fatalf("first build: %s\n%s", first.Status, f.log(first.ID))
	}
	f.cancel()
	f.eng.Wait()

	// A push happens while the service is down.
	f.write(filepath.Join(f.src, "README"), "changed\n")
	f.git("commit", "--quiet", "-am", "Pushed while down")
	head := f.git("rev-parse", "HEAD")

	ctx, cancel := context.WithCancel(context.Background())
	eng := New(config.Config{DataDir: f.data, RepoScript: repoScript}, f.st)
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); eng.Wait() }()

	deadline := time.Now().Add(20 * time.Second)
	for !f.st.CommitCovered(f.repo.ID, "main", head) {
		if time.Now().After(deadline) {
			t.Fatal("the missed push was not queued")
		}
		time.Sleep(20 * time.Millisecond)
	}
	b := f.finished(first.ID + 1)
	if b.Trigger != store.TriggerRecovery || b.Status != store.StatusSuccess || b.CommitMessage != "Pushed while down" {
		t.Errorf("recovery build: %+v", b)
	}
}

func TestRetentionPrunesOldArtifacts(t *testing.T) {
	f := newFixture(t, "echo data > \"$ARTIFACTS_DIR/out.txt\"\n")
	f.st.SetSetting(store.KeyRetention, "2")
	var ids []int64
	for i := 0; i < 3; i++ {
		id := f.run(store.NewBuild{})
		f.finished(id)
		ids = append(ids, id)
	}
	// Pruning happens right after the result is recorded.
	f.waitFor(ids[0], func(b store.Build) bool { return b.ArtifactsPruned })
	if _, err := os.Stat(f.eng.ArtifactDir("acme/app", ids[0])); !os.IsNotExist(err) {
		t.Error("the oldest build's artifacts are still on disk")
	}
	for _, id := range ids[1:] {
		if arts, _ := f.st.ListArtifacts(id); len(arts) != 1 {
			t.Errorf("build #%d lost its artifacts", id)
		}
	}
}

func TestLogMasking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	lw, err := newLogWriter(path, []string{"hunter2-long", "abc", "-----BEGIN KEY-----\nMIIEvQIBADANBg\n-----END KEY-----"})
	if err != nil {
		t.Fatal(err)
	}
	lw.Write([]byte("pass=hunter2"))
	lw.Write([]byte("-long ok\nabc stays, MIIEvQIBADANBg goes\npartial"))
	lw.Close()
	got, _ := os.ReadFile(path)
	want := "pass=*** ok\nabc stays, *** goes\npartial\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAccessHint(t *testing.T) {
	const prompt = "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n"
	cases := []struct {
		name, output string
		hasToken     bool
		want         string
	}{
		{"private repo, no token", prompt, false, "no GitHub token is saved"},
		{"token rejected", prompt, true, "rejected the saved GitHub token"},
		{"token without access to the repo", "remote: Repository not found.\nfatal: repository 'https://github.com/acme/app.git/' not found\n", true, "cannot see acme/app"},
		{"token without contents permission", "remote: Write access to repository not granted.\nfatal: unable to access 'https://github.com/acme/app.git/': The requested URL returned error: 403\n", true, "read access to Contents"},
		{"deploy key refused", "git@github.com: Permission denied (publickey).\n", false, "deploy key"},
		{"unrelated failure", "fatal: couldn't find remote ref refs/heads/gone\n", true, ""},
	}
	for _, c := range cases {
		got := accessHint(c.output, c.hasToken, "acme/app")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to mention %q", c.name, got, c.want)
		}
	}
}

// A checkout that fails for lack of access reports what to do, not the
// internal detail about the commit.
func TestCheckoutAccessFailureIsExplained(t *testing.T) {
	f := newFixture(t, "true\n")
	// An HTTPS remote that answers 401, like GitHub does for a private repo.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	f.repo.CloneURL = srv.URL + "/acme/app.git"
	f.st.UpdateRepo(f.repo, f.repo.Branches)

	b := f.finished(f.run(store.NewBuild{CommitSHA: strings.Repeat("a", 40), Trigger: store.TriggerWebhook}))
	if b.Status != store.StatusFailed || !strings.Contains(b.Message, "no GitHub token is saved") {
		t.Errorf("status %s, message %q\n%s", b.Status, b.Message, f.log(b.ID))
	}
}

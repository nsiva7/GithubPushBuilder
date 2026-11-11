package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nsiva7/GithubPushBuilder/internal/config"
	"github.com/nsiva7/GithubPushBuilder/internal/notify"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

// postTimeout caps the post script, which runs outside the build timeout.
const postTimeout = 10 * time.Minute

// timeoutUnit is the unit of the timeout settings. Tests shorten it.
var timeoutUnit = time.Minute

// run is the state of one build while it executes.
type run struct {
	e       *Engine
	b       store.Build
	repo    store.Repo
	lw      *logWriter
	ws      string            // checkout
	tmp     string            // scratch directory, holds the BUILD_ENV file
	art     string            // where the repo script puts its outputs
	secrets map[string]string // build secrets this repo may use
	extra   []string          // KEY=VALUE lines the scripts appended to $BUILD_ENV
}

func (e *Engine) execute(ab *activeBuild, b store.Build) {
	repo, err := e.st.GetRepo(b.RepoID)
	if err != nil {
		e.st.FinishBuild(b.ID, store.StatusFailed, "The repo was removed")
		return
	}
	secrets, err := e.st.SecretsForRepo(repo.ID)
	if err != nil {
		e.st.FinishBuild(b.ID, store.StatusFailed, "Cannot load build secrets: "+err.Error())
		return
	}
	hide := []string{e.token(repo), repo.WebhookSecret, e.st.Setting(store.KeySlackWebhook)}
	for _, v := range secrets {
		hide = append(hide, v)
	}
	lw, err := newLogWriter(e.LogPath(b.ID), hide)
	if err != nil {
		e.st.FinishBuild(b.ID, store.StatusFailed, "Cannot open the log file: "+err.Error())
		return
	}
	r := &run{
		e: e, b: b, repo: repo, lw: lw, secrets: secrets,
		ws:  e.workspaceDir(b.ID),
		tmp: e.tmpDir(b.ID),
		art: e.ArtifactDir(repo.FullName, b.ID),
	}

	// A re-queued build starts from scratch.
	for _, d := range []string{r.ws, r.tmp, r.art} {
		forceRemoveAll(d)
	}
	e.st.ClearArtifacts(b.ID)
	if err := errors.Join(os.MkdirAll(r.tmp, 0o700), os.MkdirAll(r.art, 0o755)); err != nil {
		lw.Notef("Cannot prepare build directories: %v", err)
		lw.Close()
		e.st.FinishBuild(b.ID, store.StatusFailed, "Cannot prepare build directories: "+err.Error())
		return
	}

	minutes := repo.TimeoutMinutes
	if minutes < 1 {
		minutes = e.st.SettingInt(store.KeyDefaultTimeout)
	}
	ctx, cancel := context.WithTimeout(ab.ctx, time.Duration(minutes)*timeoutUnit)
	defer cancel()

	lw.Notef("Build #%d of %s, branch %s, trigger %s, timeout %d min", b.ID, repo.FullName, b.Branch, b.Trigger, minutes)
	status, msg := r.steps(ctx)
	switch {
	case e.ctx.Err() != nil:
		// Leave the build as running: startup recovery re-queues it.
		lw.Notef("The service is shutting down; this build will be re-queued when it starts again")
		lw.Close()
		return
	case ab.userCancel.Load():
		status, msg = store.StatusCancelled, "Cancelled from the dashboard"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status, msg = store.StatusTimedOut, fmt.Sprintf("Timed out after %d minutes", minutes)
	}
	if status != store.StatusSuccess {
		lw.Notef("%s", msg)
	}

	// The post script always runs, whatever happened above.
	if post := e.scriptPath(repo, "post.sh"); post != "" {
		lw.Notef("=== Post script: %s (BUILD_STATUS=%s)", post, status)
		r.loadBuildEnv()
		dir := r.ws
		if _, err := os.Stat(dir); err != nil {
			dir = r.tmp // checkout never happened
		}
		pctx, pcancel := context.WithTimeout(e.ctx, postTimeout)
		err := runScript(pctx, lw, post, dir, r.env(status))
		pcancel()
		if e.ctx.Err() != nil {
			lw.Notef("The service is shutting down; this build will be re-queued when it starts again")
			lw.Close()
			return
		}
		if err != nil {
			lw.Notef("Post script failed: %s", describeExit(err))
			if status == store.StatusSuccess {
				status, msg = store.StatusFailed, "Post script failed: "+describeExit(err)
			}
		}
	}

	if n, err := r.indexArtifacts(); err != nil {
		lw.Notef("Could not index artifacts: %v", err)
	} else {
		lw.Notef("Artifacts collected: %d", n)
	}
	r.notifyFinished(status, msg)
	forceRemoveAll(r.ws)
	forceRemoveAll(r.tmp)
	lw.Notef("Finished: %s", status)
	lw.Close()

	// Recorded last, so anyone following the log sees all of it before the
	// build shows as finished.
	if err := e.st.FinishBuild(b.ID, status, msg); err != nil {
		fmt.Fprintf(os.Stderr, "build #%d: cannot record result: %v\n", b.ID, err)
	}
	e.prune(repo, b.Branch)
}

// steps runs checkout, the pre script and the repo script. It returns success
// or failed; the caller turns a cancel or timeout into the right state.
func (r *run) steps(ctx context.Context) (status, msg string) {
	r.lw.Notef("=== Checkout")
	if err := r.e.checkout(ctx, r.lw, r.repo, &r.b, r.ws); err != nil {
		return store.StatusFailed, "Checkout failed: " + err.Error()
	}
	r.lw.Notef("Checked out %s (%s)", r.b.CommitSHA, r.b.CommitMessage)
	r.commitStatus("pending", "Build #"+strconv.FormatInt(r.b.ID, 10)+" is running")

	rel := r.e.cfg.RepoScript
	script := filepath.Join(r.ws, filepath.FromSlash(rel))
	if st, err := os.Stat(script); err != nil || !st.Mode().IsRegular() {
		return store.StatusFailed, fmt.Sprintf("The repo has no build script: %s is missing at commit %.12s", rel, r.b.CommitSHA)
	}

	if pre := r.e.scriptPath(r.repo, "pre.sh"); pre != "" {
		r.lw.Notef("=== Pre script: %s", pre)
		if err := runScript(ctx, r.lw, pre, r.ws, r.env("")); err != nil {
			return store.StatusFailed, "Pre script failed (" + describeExit(err) + "); the repo script was skipped"
		}
	}
	r.loadBuildEnv()

	r.lw.Notef("=== Repo script: %s", rel)
	if err := runScript(ctx, r.lw, script, r.ws, r.env("")); err != nil {
		return store.StatusFailed, "Repo script failed: " + describeExit(err)
	}
	return store.StatusSuccess, ""
}

// env builds the environment for a script. buildStatus is set for the post script only.
func (r *run) env(buildStatus string) []string {
	env := baseEnv()
	for k, v := range r.secrets {
		env = append(env, k+"="+v)
	}
	env = append(env, r.extra...)
	// Last, so neither a secret nor $BUILD_ENV can override them.
	env = append(env,
		"CI=true",
		"REPO="+r.repo.FullName,
		"BRANCH="+r.b.Branch,
		"COMMIT_SHA="+r.b.CommitSHA,
		"BUILD_ID="+strconv.FormatInt(r.b.ID, 10),
		"WORKSPACE="+r.ws,
		"ARTIFACTS_DIR="+r.art,
		"BUILD_TMP="+r.tmp,
		"BUILD_ENV="+r.buildEnvFile(),
		"CACHE_DIR="+r.e.cacheDir(),
		"BUILD_TRIGGER="+r.b.Trigger,
		"BUILD_URL="+r.buildURL(),
	)
	if buildStatus != "" {
		env = append(env, "BUILD_STATUS="+buildStatus)
	}
	return env
}

func (r *run) buildEnvFile() string { return filepath.Join(r.tmp, "build.env") }

func (r *run) buildURL() string {
	base := r.e.BaseURL()
	if base == "" {
		return ""
	}
	return base + "/builds/" + strconv.FormatInt(r.b.ID, 10)
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loadBuildEnv reads the KEY=VALUE lines scripts appended to $BUILD_ENV. This
// is how the pre script hands variables to the repo and post scripts, which
// run as separate processes.
func (r *run) loadBuildEnv() {
	f, err := os.Open(r.buildEnvFile())
	if err != nil {
		return
	}
	defer f.Close()
	r.extra = nil
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envNameRE.MatchString(k) {
			r.lw.Notef("Ignoring a malformed line in $BUILD_ENV (want KEY=VALUE)")
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		r.extra = append(r.extra, k+"="+v)
	}
}

// indexArtifacts records every regular file under the artifact directory with
// its size and SHA-256.
func (r *run) indexArtifacts() (int, error) {
	count := 0
	err := filepath.WalkDir(r.art, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() { // directories, and no symlinks out of the tree
			return nil
		}
		rel, err := filepath.Rel(r.art, p)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		size, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		count++
		return r.e.st.AddArtifact(r.b.ID, filepath.ToSlash(rel), size, hex.EncodeToString(h.Sum(nil)))
	})
	if count == 0 {
		os.Remove(r.art) // keep the volume free of empty directories
	}
	return count, err
}

// commitStatus sets the GitHub commit status, when enabled for the repo and a token is available.
func (r *run) commitStatus(state, description string) {
	tok := r.e.token(r.repo)
	if !r.repo.CommitStatus || tok == "" || r.b.CommitSHA == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := notify.CommitStatus(ctx, r.e.cfg.GitHubAPI, tok, r.repo.FullName, r.b.CommitSHA,
		state, description, r.buildURL(), config.ServiceName)
	if err != nil {
		r.lw.Notef("GitHub commit status (%s) not set: %v", state, err)
	}
}

func (r *run) notifyFinished(status, msg string) {
	dur := time.Since(time.Unix(r.b.StartedAt, 0)).Round(time.Second)

	switch status {
	case store.StatusSuccess:
		r.commitStatus("success", "Build succeeded in "+dur.String())
	case store.StatusCancelled:
		r.commitStatus("failure", "Build cancelled")
	default:
		r.commitStatus("failure", msg)
	}

	hook := r.e.st.Setting(store.KeySlackWebhook)
	if hook == "" || status == store.StatusCancelled {
		return
	}
	icon, verb, color := ":x:", "failed", "#d1242f"
	switch status {
	case store.StatusSuccess:
		icon, verb, color = ":white_check_mark:", "succeeded", "#1a7f37"
	case store.StatusTimedOut:
		icon, verb = ":hourglass:", "timed out"
	}
	text := fmt.Sprintf("%s *%s* `%s` build #%d %s after %s", icon, r.repo.FullName, r.b.Branch, r.b.ID, verb, dur)
	if r.b.CommitSHA != "" {
		text += fmt.Sprintf("\n`%.7s` %s", r.b.CommitSHA, slackEscape(r.b.CommitMessage))
	}
	if status != store.StatusSuccess && msg != "" {
		text += "\n" + slackEscape(msg)
	}
	if u := r.buildURL(); u != "" {
		text += fmt.Sprintf("\n<%s|Open build #%d>", u, r.b.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := notify.Slack(ctx, hook, text, color); err != nil {
		r.lw.Notef("Slack notification not sent: %v", err)
	}
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func slackEscape(s string) string { return slackEscaper.Replace(s) }

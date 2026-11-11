package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

// cloneURL is where a repo is fetched from.
func cloneURL(repo store.Repo) string {
	if repo.CloneURL != "" {
		return repo.CloneURL
	}
	return "https://github.com/" + repo.FullName + ".git"
}

// token returns the GitHub token for a repo: its own, or the global one.
func (e *Engine) token(repo store.Repo) string {
	if repo.Token != "" {
		return repo.Token
	}
	return e.st.Setting(store.KeyGitHubToken)
}

// mirrorDir is the bare repository that caches a repo's objects between builds.
func (e *Engine) mirrorDir(repo store.Repo) string {
	return filepath.Join(e.cacheDir(), "git", repo.Owner(), repo.Name()+".git")
}

func (e *Engine) repoLock(id int64) *sync.Mutex {
	mu, _ := e.repoMu.LoadOrStore(id, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// gitEnv is the environment for the service's own git commands. The token is
// passed as an HTTP header scoped to the clone host; it never touches argv,
// .git/config, or the environment of the build scripts.
func (e *Engine) gitEnv(repo store.Repo) []string {
	env := baseEnv()
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new")
	}
	cfg := [][2]string{{"safe.directory", "*"}, {"advice.detachedHead", "false"}}
	if tok := e.token(repo); tok != "" {
		if u, err := url.Parse(cloneURL(repo)); err == nil && u.Scheme == "https" {
			auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
			cfg = append(cfg, [2]string{"http.https://" + u.Host + "/.extraheader", "Authorization: Basic " + auth})
		}
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// baseEnv is the service environment minus its own GPB_* configuration, which
// includes the initial admin password.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GPB_") {
			env = append(env, kv)
		}
	}
	return env
}

// gitOut runs a quick local git command and returns its trimmed output.
func gitOut(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// remoteHead asks the remote for the current head of a branch ("" if the branch is gone).
func (e *Engine) remoteHead(ctx context.Context, repo store.Repo, branch string) (string, error) {
	var out bytes.Buffer
	err := runProc(ctx, &out, e.cfg.DataDir, e.gitEnv(repo), "git", "ls-remote", cloneURL(repo), "refs/heads/"+branch)
	if err != nil {
		return "", errors.New(firstLine(out.String(), describeExit(err)))
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "refs/heads/"+branch {
			return f[0], nil
		}
	}
	return "", nil
}

// checkout puts the build's commit into a fresh workspace. It fetches into the
// repo's cache, resolves the commit for manual runs of a branch head, and
// updates b with what it found.
func (e *Engine) checkout(ctx context.Context, lw io.Writer, repo store.Repo, b *store.Build, ws string) error {
	src := cloneURL(repo)
	env := e.gitEnv(repo)
	mirror := e.mirrorDir(repo)
	ref := "refs/heads/" + b.Branch
	// Keep a copy of what git prints, to turn an access problem into advice.
	var said bytes.Buffer
	out := io.MultiWriter(lw, &said)
	git := func(dir string, args ...string) error {
		return runProc(ctx, out, dir, env, "git", args...)
	}
	has := func(sha string) bool {
		_, err := gitOut(ctx, env, mirror, "cat-file", "-e", sha+"^{commit}")
		return err == nil
	}

	lock := e.repoLock(repo.ID)
	lock.Lock()
	err := func() error {
		if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
			if err := os.MkdirAll(mirror, 0o755); err != nil {
				return err
			}
			if err := git(mirror, "init", "--quiet", "--bare"); err != nil {
				return fmt.Errorf("git init: %s", describeExit(err))
			}
		}
		fetchErr := git(mirror, "fetch", "--quiet", "--force", src, "+"+ref+":"+ref)
		if b.CommitSHA == "" {
			if fetchErr != nil {
				return fmt.Errorf("cannot fetch branch %s: %s", b.Branch, describeExit(fetchErr))
			}
			sha, err := gitOut(ctx, env, mirror, "rev-parse", ref)
			if err != nil {
				return fmt.Errorf("cannot resolve branch %s", b.Branch)
			}
			b.CommitSHA = sha
		} else if !has(b.CommitSHA) {
			// Not reachable from the branch any more (force push, or the branch
			// is gone): ask for the commit itself.
			if err := git(mirror, "fetch", "--quiet", src, b.CommitSHA); err != nil || !has(b.CommitSHA) {
				return fmt.Errorf("commit %.12s is not available from %s", b.CommitSHA, repo.FullName)
			}
		}
		// Keep the commit referenced so the clone below is sure to carry it.
		return git(mirror, "update-ref", fmt.Sprintf("refs/builds/%d", b.ID), b.CommitSHA)
	}()
	if err == nil {
		if cerr := git(e.cfg.DataDir, "clone", "--quiet", "--no-checkout", mirror, ws); cerr != nil {
			err = fmt.Errorf("git clone: %s", describeExit(cerr))
		}
		git(mirror, "update-ref", "-d", fmt.Sprintf("refs/builds/%d", b.ID))
	}
	lock.Unlock()
	if err != nil {
		if hint := accessHint(said.String(), e.token(repo) != "", repo.FullName); hint != "" {
			return errors.New(hint)
		}
		return err
	}

	if b.CommitMessage == "" || b.Author == "" {
		if out, err := gitOut(ctx, env, mirror, "log", "-1", "--format=%an%n%s", b.CommitSHA); err == nil {
			author, subject, _ := strings.Cut(out, "\n")
			if b.Author == "" {
				b.Author = author
			}
			if b.CommitMessage == "" {
				b.CommitMessage = subject
			}
		}
	}
	if err := e.st.SetBuildCommit(b.ID, b.CommitSHA, b.CommitMessage, b.Author); err != nil {
		return err
	}

	if err := git(ws, "checkout", "--quiet", "--detach", b.CommitSHA); err != nil {
		return fmt.Errorf("git checkout: %s", describeExit(err))
	}
	// Point origin at the real remote instead of the local cache.
	if err := git(ws, "remote", "set-url", "origin", src); err != nil {
		return fmt.Errorf("git remote: %s", describeExit(err))
	}
	if _, err := os.Stat(filepath.Join(ws, ".gitmodules")); err == nil {
		if err := git(ws, "submodule", "update", "--init", "--recursive", "--quiet"); err != nil {
			return fmt.Errorf("git submodule update: %s", describeExit(err))
		}
	}
	return nil
}

// accessHint explains what to do when git's output shows the remote refused
// access. It returns "" when the output is about something else.
func accessHint(gitOutput string, hasToken bool, repo string) string {
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(gitOutput, p) {
				return true
			}
		}
		return false
	}
	const where = "under Settings, or on the repo's page for a token of its own"
	switch {
	case has("could not read Username", "Authentication failed", "Invalid username or token", "could not read Password"):
		if !hasToken {
			return "GitHub asked for a login and no GitHub token is saved. Add one " + where + ", then press Retry"
		}
		return "GitHub rejected the saved GitHub token. Check that it was pasted completely and has not expired, save it again " +
			where + ", then press Retry"
	case has("Repository not found", "repository not found"):
		if !hasToken {
			return "GitHub cannot find " + repo + " without a login. If the repo is private, add a GitHub token " + where
		}
		return "The saved GitHub token cannot see " + repo + ". Give the token access to this repo " +
			"(fine-grained tokens: Repository access) and check the repo name"
	case has("Write access to repository not granted", "The requested URL returned error: 403"):
		return "The saved GitHub token is not allowed to read " + repo + ". It needs read access to Contents"
	case has("Permission denied (publickey)", "Host key verification failed"):
		return "The SSH login to the remote was refused. Check the deploy key mounted in /home/builder/.ssh and that it was added to " + repo
	case has("Could not resolve host", "Failed to connect to", "Connection timed out"):
		return "The remote could not be reached from the build server. Check its network and DNS"
	}
	return ""
}

func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

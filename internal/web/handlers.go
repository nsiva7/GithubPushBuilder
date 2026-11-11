package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nsiva7/GithubPushBuilder/internal/engine"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

const historyPageSize = 25

// ---- status page ----

type target struct {
	RepoID int64
	Repo   string
	Branch string
}

type indexData struct {
	Running, Queued, History []store.Build
	Repos                    []store.Repo
	Targets                  []target
	RepoFilter               int64
	Page, Pages              int
	MaxParallel              int
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := indexData{Page: 1, MaxParallel: s.st.SettingInt(store.KeyMaxParallel)}
	d.RepoFilter, _ = strconv.ParseInt(q.Get("repo"), 10, 64)
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		d.Page = p
	}
	var err error
	if d.Running, err = s.st.BuildsByStatus(store.StatusRunning); err != nil {
		s.fail(w, err)
		return
	}
	if d.Queued, err = s.st.BuildsByStatus(store.StatusQueued); err != nil {
		s.fail(w, err)
		return
	}
	var total int
	if d.History, total, err = s.st.History(d.RepoFilter, historyPageSize, (d.Page-1)*historyPageSize); err != nil {
		s.fail(w, err)
		return
	}
	d.Pages = max(1, (total+historyPageSize-1)/historyPageSize)
	if d.Repos, err = s.st.ListRepos(); err != nil {
		s.fail(w, err)
		return
	}
	for _, repo := range d.Repos {
		for _, b := range repo.Branches {
			d.Targets = append(d.Targets, target{repo.ID, repo.FullName, b})
		}
	}
	if q.Get("fragment") == "1" {
		s.exec(w, "index", "live", pageData{CSRF: info(r).CSRF, D: d}) // no flash: leave it for a full page
		return
	}
	s.render(w, r, "index", "Builds", "builds", d)
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (s *Server) runManual(w http.ResponseWriter, r *http.Request) {
	idStr, branch, _ := strings.Cut(r.PostFormValue("target"), "|")
	repoID, _ := strconv.ParseInt(idStr, 10, 64)
	sha := strings.ToLower(strings.TrimSpace(r.PostFormValue("sha")))
	switch {
	case !s.st.BranchAllowed(repoID, branch):
		s.flash(w, "err", "Pick a registered repo and branch.")
	case sha != "" && !fullSHA.MatchString(sha):
		s.flash(w, "err", "The commit must be a full 40-character SHA, or empty for the branch head.")
	default:
		id, err := s.eng.Enqueue(store.NewBuild{RepoID: repoID, Branch: branch, CommitSHA: sha, Trigger: store.TriggerManual})
		if err != nil {
			s.fail(w, err)
			return
		}
		s.back(w, r, fmt.Sprintf("/builds/%d", id))
		return
	}
	s.back(w, r, "/")
}

// ---- build page ----

type buildData struct {
	Build     store.Build
	Artifacts []store.Artifact
	CommitURL string
	RepoID    int64
}

func (s *Server) buildPage(w http.ResponseWriter, r *http.Request) {
	b, err := s.st.GetBuild(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := buildData{Build: b, RepoID: b.RepoID}
	if d.Artifacts, err = s.st.ListArtifacts(b.ID); err != nil {
		s.fail(w, err)
		return
	}
	if repo, err := s.st.GetRepo(b.RepoID); err == nil && repo.CloneURL == "" && b.CommitSHA != "" {
		d.CommitURL = "https://github.com/" + repo.FullName + "/commit/" + b.CommitSHA
	}
	s.render(w, r, "build", fmt.Sprintf("Build #%d", b.ID), "builds", d)
}

func (s *Server) buildCancel(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	switch err := s.eng.Cancel(id); {
	case errors.Is(err, engine.ErrNotActive):
		s.flash(w, "err", "Build #%d has already finished.", id)
	case err != nil:
		s.fail(w, err)
		return
	default:
		s.flash(w, "ok", "Cancelling build #%d.", id)
	}
	s.back(w, r, fmt.Sprintf("/builds/%d", id))
}

func (s *Server) buildRetry(w http.ResponseWriter, r *http.Request) {
	b, err := s.st.GetBuild(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if b.Active() {
		s.flash(w, "err", "Build #%d is still %s.", b.ID, b.Status)
		s.back(w, r, fmt.Sprintf("/builds/%d", b.ID))
		return
	}
	id, err := s.eng.Enqueue(store.NewBuild{
		RepoID: b.RepoID, Branch: b.Branch, CommitSHA: b.CommitSHA, CommitMessage: b.CommitMessage,
		Author: b.Author, Trigger: store.TriggerRetry, RetryOf: b.ID,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.back(w, r, fmt.Sprintf("/builds/%d", id))
}

func (s *Server) buildLog(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if _, err := s.st.GetBuild(id); err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.eng.LogPath(id))
	if err != nil {
		http.Error(w, "This build has no log yet.", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="build-%d.log"`, id))
	}
	io.Copy(w, f)
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	b, err := s.st.GetBuild(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Only files that were indexed can be served, and os.Root keeps the open
	// inside the build's artifact directory even if a symlink is planted there.
	a, err := s.st.GetArtifact(b.ID, r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(s.eng.ArtifactDir(b.Repo, b.ID))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(a.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	name := strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "").Replace(path.Base(a.Path))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// buildEvents streams a build's log as server-sent events by following the
// log file. Events: default (a JSON string of new log text, id = byte offset),
// "reset" (the log restarted), "status" (state changed), "done" (finished).
func (s *Server) buildEvents(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	b, err := s.st.GetBuild(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)

	offset, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	status := b.Status
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
	flusher.Flush()

	buf := make([]byte, 64<<10)
	// read sends whatever the log holds past offset. While the build runs it
	// stops at the last newline so a line is never split mid-character.
	read := func(final bool) bool {
		f, err := os.Open(s.eng.LogPath(id))
		if err != nil {
			return false
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil && st.Size() < offset {
			offset = 0 // the build was re-queued and its log started over
			fmt.Fprint(w, "event: reset\ndata: \n\n")
		}
		sent := false
		for {
			n, _ := f.ReadAt(buf, offset)
			chunk := buf[:n]
			if !final || n == len(buf) {
				if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
					chunk = chunk[:i+1]
				} else if n < len(buf) {
					chunk = nil
				}
			}
			if len(chunk) == 0 {
				break
			}
			offset += int64(len(chunk))
			data, _ := json.Marshal(string(chunk))
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", offset, data)
			sent = true
		}
		if sent {
			flusher.Flush()
		}
		return sent
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	idle := 0
	for {
		if !read(false) {
			cur, err := s.st.GetBuild(id)
			if err != nil {
				return
			}
			if cur.Status != status {
				status = cur.Status
				if cur.Active() {
					fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
					flusher.Flush()
				}
			}
			if !cur.Active() {
				// The result is recorded after the log is closed, so one last
				// read is guaranteed to see all of it.
				read(true)
				fmt.Fprintf(w, "event: status\ndata: %s\n\nevent: done\ndata: %s\n\n", status, status)
				flusher.Flush()
				return
			}
			if idle++; idle%30 == 0 {
				fmt.Fprint(w, ": keep-alive\n\n")
				flusher.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// ---- repos ----

var (
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9_.-]+$`)
	splitRE    = regexp.MustCompile(`[\s,]+`)
)

func validRepoName(name string) bool {
	if !repoNameRE.MatchString(name) || len(name) > 140 || strings.HasSuffix(name, ".git") {
		return false
	}
	_, repo, _ := strings.Cut(name, "/")
	return repo != "." && repo != ".."
}

func validBranch(b string) bool {
	if b == "" || len(b) > 200 || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.Contains(b, "..") || strings.ContainsAny(b, " ~^:?*[\\|") {
		return false
	}
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func parseBranches(raw string) ([]string, error) {
	var out []string
	for _, b := range splitRE.Split(strings.TrimSpace(raw), -1) {
		if b == "" {
			continue
		}
		if !validBranch(b) {
			return nil, fmt.Errorf("%q is not a valid branch name", b)
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, errors.New("list at least one branch")
	}
	return out, nil
}

// validCloneURL accepts the transports that make sense here and nothing that
// git could read as an option or a command (ext::, leading dash).
func validCloneURL(u string) bool {
	if u == "" {
		return true
	}
	if strings.ContainsAny(u, " \t\r\n") {
		return false
	}
	for _, p := range []string{"https://", "ssh://", "git@", "file:///", "/"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

type reposData struct {
	Repos   []store.Repo
	HookURL string
}

func (s *Server) hookURL() string {
	if base := s.eng.BaseURL(); base != "" {
		return base + "/hooks/github"
	}
	return "https://<your-host>/hooks/github"
}

func (s *Server) reposPage(w http.ResponseWriter, r *http.Request) {
	repos, err := s.st.ListRepos()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "repos", "Repos", "repos", reposData{Repos: repos, HookURL: s.hookURL()})
}

type repoData struct {
	Repo           store.Repo
	HookURL        string
	NewSecret      string // shown once, right after it was generated
	DefaultTimeout int
	RepoScript     string
	ScriptDir      string
}

func (s *Server) renderRepo(w http.ResponseWriter, r *http.Request, repo store.Repo, newSecret string) {
	s.render(w, r, "repo", repo.FullName, "repos", repoData{
		Repo: repo, HookURL: s.hookURL(), NewSecret: newSecret,
		DefaultTimeout: s.st.SettingInt(store.KeyDefaultTimeout),
		RepoScript:     s.cfg.RepoScript,
		ScriptDir:      filepath.Join(s.cfg.DataDir, "scripts", repo.Owner(), repo.Name()),
	})
}

func (s *Server) repoCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("full_name"))
	name = strings.TrimSuffix(strings.TrimPrefix(name, "https://github.com/"), "/")
	branches, berr := parseBranches(r.PostFormValue("branches"))
	switch {
	case !validRepoName(name):
		s.flash(w, "err", "Enter the repo as owner/name.")
	case berr != nil:
		s.flash(w, "err", "Branches: %v.", berr)
	default:
		if _, err := s.st.GetRepoByName(name); err == nil {
			s.flash(w, "err", "%s is already registered.", name)
			break
		}
		secret := randomHex(32)
		id, err := s.st.CreateRepo(store.Repo{FullName: name, WebhookSecret: secret, CommitStatus: true, Enabled: true}, branches)
		if err != nil {
			s.fail(w, err)
			return
		}
		repo, err := s.st.GetRepo(id)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.renderRepo(w, r, repo, secret)
		return
	}
	s.back(w, r, "/repos")
}

func (s *Server) repoPage(w http.ResponseWriter, r *http.Request) {
	repo, err := s.st.GetRepo(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderRepo(w, r, repo, "")
}

func (s *Server) repoUpdate(w http.ResponseWriter, r *http.Request) {
	repo, err := s.st.GetRepo(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	here := fmt.Sprintf("/repos/%d", repo.ID)
	branches, berr := parseBranches(r.PostFormValue("branches"))
	cloneURL := strings.TrimSpace(r.PostFormValue("clone_url"))
	timeout, terr := strconv.Atoi(strings.TrimSpace(r.PostFormValue("timeout_minutes")))
	if strings.TrimSpace(r.PostFormValue("timeout_minutes")) == "" {
		timeout, terr = 0, nil
	}
	switch {
	case berr != nil:
		s.flash(w, "err", "Branches: %v.", berr)
	case !validCloneURL(cloneURL):
		s.flash(w, "err", "The clone URL must start with https://, ssh://, git@ or be an absolute path.")
	case terr != nil || timeout < 0 || timeout > 1440:
		s.flash(w, "err", "The timeout must be between 1 and 1440 minutes, or empty for the default.")
	default:
		repo.CloneURL = cloneURL
		repo.TimeoutMinutes = timeout
		repo.CommitStatus = r.PostFormValue("commit_status") == "on"
		repo.Enabled = r.PostFormValue("enabled") == "on"
		if tok := strings.TrimSpace(r.PostFormValue("token")); tok != "" {
			repo.Token = tok
		} else if r.PostFormValue("clear_token") == "on" {
			repo.Token = ""
		}
		if err := s.st.UpdateRepo(repo, branches); err != nil {
			s.fail(w, err)
			return
		}
		s.flash(w, "ok", "Saved.")
	}
	s.back(w, r, here)
}

func (s *Server) repoNewSecret(w http.ResponseWriter, r *http.Request) {
	repo, err := s.st.GetRepo(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	secret := randomHex(32)
	if err := s.st.SetWebhookSecret(repo.ID, secret); err != nil {
		s.fail(w, err)
		return
	}
	repo.WebhookSecret = secret
	s.renderRepo(w, r, repo, secret)
}

func (s *Server) repoDelete(w http.ResponseWriter, r *http.Request) {
	repo, err := s.st.GetRepo(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if n, err := s.st.ActiveCount(repo.ID); err != nil {
		s.fail(w, err)
		return
	} else if n > 0 {
		s.flash(w, "err", "%s has %d queued or running build(s). Cancel them first.", repo.FullName, n)
		s.back(w, r, fmt.Sprintf("/repos/%d", repo.ID))
		return
	}
	ids, err := s.st.BuildIDs(repo.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.DeleteRepo(repo.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.eng.RemoveRepoFiles(repo, ids)
	s.flash(w, "ok", "Removed %s with its builds, logs and artifacts.", repo.FullName)
	s.back(w, r, "/repos")
}

// ---- build secrets ----

var (
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// reserved names are set by the service or the shell and can't be secrets.
	reserved = map[string]bool{
		"REPO": true, "BRANCH": true, "COMMIT_SHA": true, "BUILD_ID": true, "WORKSPACE": true,
		"ARTIFACTS_DIR": true, "BUILD_STATUS": true, "BUILD_TMP": true, "BUILD_ENV": true,
		"CACHE_DIR": true, "BUILD_TRIGGER": true, "BUILD_URL": true, "CI": true,
		"PATH": true, "HOME": true, "USER": true, "SHELL": true, "PWD": true,
	}
)

type secretsData struct {
	Secrets []store.Secret
	Repos   []store.Repo
}

func (s *Server) secretsPage(w http.ResponseWriter, r *http.Request) {
	secrets, err := s.st.ListSecrets()
	if err != nil {
		s.fail(w, err)
		return
	}
	repos, err := s.st.ListRepos()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "secrets", "Build secrets", "secrets", secretsData{secrets, repos})
}

func (s *Server) secretCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := strings.TrimRight(r.PostFormValue("value"), "\r\n")
	all := r.PostFormValue("scope") == "all"
	var repoIDs []int64
	for _, v := range r.PostForm["repo"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			repoIDs = append(repoIDs, id)
		}
	}
	switch {
	case !envNameRE.MatchString(name):
		s.flash(w, "err", "The name must be a valid environment variable name, like KEYSTORE_PASSWORD.")
	case reserved[name] || strings.HasPrefix(name, "GPB_") || strings.HasPrefix(name, "GIT_"):
		s.flash(w, "err", "%s is reserved. Pick another name.", name)
	case value == "":
		s.flash(w, "err", "The value is empty.")
	case !all && len(repoIDs) == 0:
		s.flash(w, "err", "Pick the repos that may use this secret, or choose all repos.")
	default:
		if err := s.st.CreateSecret(name, value, all, repoIDs); err != nil {
			s.fail(w, err)
			return
		}
		s.flash(w, "ok", "Secret %s added. It is used from the next build.", name)
	}
	s.back(w, r, "/secrets")
}

func (s *Server) secretDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteSecret(pathID(r)); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, "ok", "Secret deleted.")
	s.back(w, r, "/secrets")
}

// ---- settings ----

type settingsData struct {
	MaxParallel, MaxWaiting, DefaultTimeout, Retention int
	BaseURL                                            string
	HasSlack, HasToken                                 bool
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings", "Settings", "settings", settingsData{
		MaxParallel:    s.st.SettingInt(store.KeyMaxParallel),
		MaxWaiting:     s.st.SettingInt(store.KeyMaxWaiting),
		DefaultTimeout: s.st.SettingInt(store.KeyDefaultTimeout),
		Retention:      s.st.SettingInt(store.KeyRetention),
		BaseURL:        s.eng.BaseURL(),
		HasSlack:       s.st.Setting(store.KeySlackWebhook) != "",
		HasToken:       s.st.Setting(store.KeyGitHubToken) != "",
	})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	type intField struct {
		key, form, label string
		max              int
	}
	fields := []intField{
		{store.KeyMaxParallel, "max_parallel_builds", "Parallel builds", 64},
		{store.KeyMaxWaiting, "max_waiting_per_branch", "Waiting pushes kept per branch", 100},
		{store.KeyDefaultTimeout, "default_timeout_minutes", "Default timeout", 1440},
		{store.KeyRetention, "retention_builds", "Builds kept per branch", 10000},
	}
	values := map[string]string{}
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(f.form)))
		if err != nil || n < 1 || n > f.max {
			s.flash(w, "err", "%s must be a number between 1 and %d.", f.label, f.max)
			s.back(w, r, "/settings")
			return
		}
		values[f.key] = strconv.Itoa(n)
	}

	base := strings.TrimRight(strings.TrimSpace(r.PostFormValue("base_url")), "/")
	if base != "" {
		if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			s.flash(w, "err", "The public URL must look like https://builds.example.com.")
			s.back(w, r, "/settings")
			return
		}
	}
	values[store.KeyBaseURL] = base

	// Write-only fields: empty keeps the stored value, the checkbox clears it.
	slack := strings.TrimSpace(r.PostFormValue("slack_webhook_url"))
	switch {
	case slack != "" && !strings.HasPrefix(slack, "https://"):
		s.flash(w, "err", "The Slack webhook URL must start with https://.")
		s.back(w, r, "/settings")
		return
	case slack != "":
		values[store.KeySlackWebhook] = slack
	case r.PostFormValue("clear_slack") == "on":
		values[store.KeySlackWebhook] = ""
	}
	if tok := strings.TrimSpace(r.PostFormValue("github_token")); tok != "" {
		values[store.KeyGitHubToken] = tok
	} else if r.PostFormValue("clear_token") == "on" {
		values[store.KeyGitHubToken] = ""
	}

	for k, v := range values {
		if err := s.st.SetSetting(k, v); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.eng.Wake() // a higher parallel limit can start a waiting build right away
	s.flash(w, "ok", "Saved. Queue settings apply from the next build.")
	s.back(w, r, "/settings")
}

func (s *Server) passwordChange(w http.ResponseWriter, r *http.Request) {
	next := r.PostFormValue("new_password")
	switch {
	case !checkPassword(s.st, r.PostFormValue("current_password")):
		s.flash(w, "err", "The current password is wrong.")
	case next != r.PostFormValue("confirm_password"):
		s.flash(w, "err", "The new passwords don't match.")
	default:
		if err := SetPassword(s.st, next); err != nil {
			s.flash(w, "err", "%s.", capitalize(err.Error()))
			break
		}
		s.st.DeleteSessionsExcept(info(r).TokenHash)
		s.flash(w, "ok", "Password changed. Other sessions were signed out.")
	}
	s.back(w, r, "/settings")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

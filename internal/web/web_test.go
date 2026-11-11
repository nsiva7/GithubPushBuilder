package web

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nsiva7/GithubPushBuilder/internal/config"
	"github.com/nsiva7/GithubPushBuilder/internal/engine"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
	"github.com/nsiva7/GithubPushBuilder/internal/webhook"
)

const password = "test-password-123"

type site struct {
	t   *testing.T
	st  *store.Store
	eng *engine.Engine
	srv *httptest.Server
	c   *http.Client // keeps cookies, does not follow redirects
}

func newSite(t *testing.T) *site {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir(), RepoScript: config.DefaultRepoScript, InsecureCookie: true}
	st, err := store.Open(filepath.Join(cfg.DataDir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureAdminPassword(st, password); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	eng := engine.New(cfg, st)
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := New(cfg, st, eng, &webhook.Handler{Store: st, Queue: eng})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	jar, _ := cookiejar.New(nil)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		eng.Wait()
		st.Close()
	})
	return &site{t: t, st: st, eng: eng, srv: srv, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (s *site) get(path string) (int, string) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+path, nil)
	req.Header.Set("Accept", "text/html")
	return s.do(req)
}

func (s *site) post(path string, form url.Values) (int, string) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.do(req)
}

func (s *site) do(req *http.Request) (int, string) {
	s.t.Helper()
	resp, err := s.c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	b := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, buf.String()
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (s *site) login() string {
	s.t.Helper()
	if code, _ := s.post("/login", url.Values{"password": {password}}); code != http.StatusSeeOther {
		s.t.Fatalf("login: %d", code)
	}
	_, body := s.get("/settings")
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		s.t.Fatal("no CSRF token on the settings page")
	}
	return m[1]
}

func TestEverythingButWebhookAndLoginNeedsASession(t *testing.T) {
	s := newSite(t)
	for _, p := range []string{"/", "/repos", "/repos/1", "/secrets", "/settings", "/builds/1", "/builds/1/log", "/builds/1/artifacts/x", "/anything"} {
		if code, _ := s.get(p); code != http.StatusSeeOther {
			t.Errorf("GET %s without a session: %d, want a redirect to the login page", p, code)
		}
	}
	for _, p := range []string{"/run", "/repos", "/settings", "/settings/password", "/builds/1/cancel", "/logout"} {
		if code, _ := s.post(p, url.Values{}); code != http.StatusUnauthorized {
			t.Errorf("POST %s without a session: %d, want 401", p, code)
		}
	}
	if code, _ := s.get("/login"); code != http.StatusOK {
		t.Errorf("login page: %d", code)
	}
	// The webhook answers on its own terms (here: no valid signature).
	if code, _ := s.post("/hooks/github", url.Values{}); code == http.StatusUnauthorized || code == http.StatusSeeOther {
		t.Errorf("the webhook is behind the login: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	s := newSite(t)
	for i := 0; i < loginMaxFails; i++ {
		if code, _ := s.post("/login", url.Values{"password": {"wrong"}}); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	if code, _ := s.post("/login", url.Values{"password": {password}}); code != http.StatusTooManyRequests {
		t.Errorf("after %d failures even the right password must wait: got %d", loginMaxFails, code)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	s := newSite(t)
	resp, err := s.c.PostForm(s.srv.URL+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			found = true
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie flags: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
			}
		}
	}
	if !found {
		t.Error("no session cookie set")
	}
}

func TestPostsNeedTheCSRFToken(t *testing.T) {
	s := newSite(t)
	csrf := s.login()
	form := url.Values{"full_name": {"acme/app"}, "branches": {"main"}}
	if code, _ := s.post("/repos", form); code != http.StatusForbidden {
		t.Errorf("without token: %d, want 403", code)
	}
	form.Set("csrf", csrf)
	code, body := s.post("/repos", form)
	if code != http.StatusOK || !strings.Contains(body, "copy it now") {
		t.Fatalf("with token: %d", code)
	}
	repo, err := s.st.GetRepoByName("acme/app")
	if err != nil || len(repo.WebhookSecret) != 64 || !strings.Contains(body, repo.WebhookSecret) {
		t.Errorf("repo not created with a shown-once secret: %v", err)
	}
	// Later views never show the secret again.
	if _, body := s.get("/repos/1"); strings.Contains(body, repo.WebhookSecret) {
		t.Error("the webhook secret is shown on a later page view")
	}
}

func TestSecretValuesAreNeverShown(t *testing.T) {
	s := newSite(t)
	csrf := s.login()
	s.post("/secrets", url.Values{"csrf": {csrf}, "name": {"API_KEY"}, "value": {"very-secret-value"}, "scope": {"all"}})
	_, body := s.get("/secrets")
	if !strings.Contains(body, "API_KEY") || strings.Contains(body, "very-secret-value") {
		t.Error("the secrets page should list the name and never the value")
	}
	s.post("/secrets", url.Values{"csrf": {csrf}, "name": {"BUILD_ID"}, "value": {"x"}, "scope": {"all"}})
	if list, _ := s.st.ListSecrets(); len(list) != 1 {
		t.Errorf("a reserved name was accepted as a secret: %+v", list)
	}
}

func TestSettingsApplyWithoutRestart(t *testing.T) {
	s := newSite(t)
	csrf := s.login()
	code, _ := s.post("/settings", url.Values{"csrf": {csrf}, "max_parallel_builds": {"3"}, "max_waiting_per_branch": {"2"},
		"default_timeout_minutes": {"45"}, "retention_builds": {"5"}, "base_url": {"https://builds.example.com/"}})
	if code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	if s.st.SettingInt(store.KeyMaxParallel) != 3 || s.st.SettingInt(store.KeyMaxWaiting) != 2 ||
		s.st.SettingInt(store.KeyDefaultTimeout) != 45 || s.eng.BaseURL() != "https://builds.example.com" {
		t.Error("settings were not stored")
	}
	s.post("/settings", url.Values{"csrf": {csrf}, "max_parallel_builds": {"0"}, "max_waiting_per_branch": {"2"},
		"default_timeout_minutes": {"45"}, "retention_builds": {"5"}})
	if s.st.SettingInt(store.KeyMaxParallel) != 3 {
		t.Error("an invalid value overwrote the setting")
	}
}

func TestPasswordChangeSignsOutOtherSessions(t *testing.T) {
	s := newSite(t)
	other := newClientFor(s)
	other.PostForm(s.srv.URL+"/login", url.Values{"password": {password}})
	csrf := s.login()
	s.post("/settings/password", url.Values{"csrf": {csrf}, "current_password": {password},
		"new_password": {"another-password-9"}, "confirm_password": {"another-password-9"}})
	if code, _ := s.get("/settings"); code != http.StatusOK {
		t.Errorf("the session that changed the password was signed out: %d", code)
	}
	resp, _ := other.Get(s.srv.URL + "/settings")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("another session survived the password change")
	}
	if !checkPassword(s.st, "another-password-9") || checkPassword(s.st, password) {
		t.Error("the password was not changed")
	}
}

func newClientFor(s *site) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestArtifactDownloadStaysInsideTheBuild(t *testing.T) {
	s := newSite(t)
	s.login()
	repoID, _ := s.st.CreateRepo(store.Repo{FullName: "acme/app", WebhookSecret: "x"}, []string{"main"})
	id, _, _ := s.st.Enqueue(store.NewBuild{RepoID: repoID, Branch: "main", Trigger: store.TriggerManual}, 1)
	s.eng.Cancel(id) // keep the engine from picking it up

	dir := s.eng.ArtifactDir("acme/app", id)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "app.apk"), []byte("apk-bytes"), 0o644)
	secret := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(secret, []byte("outside"), 0o644)
	os.Symlink(secret, filepath.Join(dir, "link"))
	s.st.AddArtifact(id, "app.apk", 9, "abc")
	s.st.AddArtifact(id, "link", 7, "def")               // indexed by mistake: still must not escape
	s.st.AddArtifact(id, "../../../db.sqlite", 1, "ghi") // a hostile row

	if code, body := s.get("/builds/1/artifacts/app.apk"); code != http.StatusOK || body != "apk-bytes" {
		t.Errorf("download: %d %q", code, body)
	}
	for _, p := range []string{"link", "..%2F..%2F..%2Fdb.sqlite", "missing.bin"} {
		if code, body := s.get("/builds/1/artifacts/" + p); code == http.StatusOK {
			t.Errorf("%s was served: %q", p, body)
		}
	}
}

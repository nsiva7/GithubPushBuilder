package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

// A build with Slack and commit statuses configured reports pending, then the
// result, and never prints the token or webhook URL in its log.
func TestSlackAndCommitStatus(t *testing.T) {
	var mu sync.Mutex
	var statuses []string
	var slack []string
	var auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/slack/") {
			slack = append(slack, string(body))
			return
		}
		var p struct{ State, Context, Target_URL string }
		json.Unmarshal(body, &p)
		statuses = append(statuses, r.URL.Path+" "+p.State+" "+p.Context)
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer api.Close()

	f := newFixture(t, "echo token=$GH; echo ok\n")
	f.eng.cfg.GitHubAPI = api.URL
	f.eng.cfg.BaseURL = "https://builds.example.com"
	f.st.SetSetting(store.KeySlackWebhook, api.URL+"/slack/T000/SECRETPART")
	f.st.SetSetting(store.KeyGitHubToken, "fake-token-for-tests")
	f.st.CreateSecret("GH", "fake-token-for-tests", true, nil)
	f.repo.CommitStatus = true
	f.st.UpdateRepo(f.repo, f.repo.Branches)
	sha := f.git("rev-parse", "HEAD")

	id := f.run(store.NewBuild{CommitSHA: sha, Trigger: store.TriggerWebhook})
	b := f.finished(id)
	if b.Status != store.StatusSuccess {
		t.Fatalf("status %s\n%s", b.Status, f.log(id))
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/repos/acme/app/statuses/" + sha + " pending GithubPushBuilder",
		"/repos/acme/app/statuses/" + sha + " success GithubPushBuilder",
	}
	if strings.Join(statuses, "\n") != strings.Join(want, "\n") {
		t.Errorf("commit statuses:\n%s\nwant:\n%s", strings.Join(statuses, "\n"), strings.Join(want, "\n"))
	}
	if auth != "Bearer fake-token-for-tests" {
		t.Errorf("authorization header: %q", auth)
	}
	if len(slack) != 1 {
		t.Fatalf("slack messages: %d, want 1", len(slack))
	}
	for _, part := range []string{"acme/app", "main", "succeeded", sha[:7], "Initial commit", "https://builds.example.com/builds/1"} {
		if !strings.Contains(slack[0], part) {
			t.Errorf("slack message is missing %q: %s", part, slack[0])
		}
	}
	if log := f.log(id); strings.Contains(log, "fake-token-for-tests") || strings.Contains(log, "SECRETPART") {
		t.Errorf("a credential leaked into the log\n%s", log)
	}
}

func TestFailedBuildReportsFailure(t *testing.T) {
	var mu sync.Mutex
	var states []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct{ State string }
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		states = append(states, p.State)
		mu.Unlock()
	}))
	defer api.Close()

	f := newFixture(t, "exit 1\n")
	f.eng.cfg.GitHubAPI = api.URL
	f.repo.Token, f.repo.CommitStatus = "repo-token-123", true
	f.st.UpdateRepo(f.repo, f.repo.Branches)
	f.finished(f.run(store.NewBuild{}))
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(states, ",") != "pending,failure" {
		t.Errorf("states: %v", states)
	}
}

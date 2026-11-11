package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

const secret = "hook-secret"

type fakeQueue struct{ got []store.NewBuild }

func (q *fakeQueue) Enqueue(nb store.NewBuild) (int64, error) {
	q.got = append(q.got, nb)
	return int64(len(q.got)), nil
}

func setup(t *testing.T) (*Handler, *fakeQueue) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateRepo(store.Repo{FullName: "acme/app", WebhookSecret: secret, Enabled: true}, []string{"main"}); err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{}
	return &Handler{Store: st, Queue: q}, q
}

func sign(key, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func push(repo, ref, sha string) string {
	return `{"ref":"` + ref + `","after":"` + sha + `","repository":{"full_name":"` + repo + `"},` +
		`"head_commit":{"message":"Fix login\n\nDetails","author":{"name":"Dana"}},"pusher":{"name":"dana"}}`
}

func send(h *Handler, event, delivery, body, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", signature)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func TestPushToAllowlistedBranchIsQueued(t *testing.T) {
	h, q := setup(t)
	body := push("acme/app", "refs/heads/main", sha)
	rec := send(h, "push", "d-1", body, sign(secret, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s, want 202", rec.Code, rec.Body)
	}
	if len(q.got) != 1 {
		t.Fatalf("queued %d builds, want 1", len(q.got))
	}
	nb := q.got[0]
	if nb.Branch != "main" || nb.CommitSHA != sha || nb.CommitMessage != "Fix login" || nb.Author != "Dana" || nb.Trigger != store.TriggerWebhook {
		t.Errorf("unexpected build: %+v", nb)
	}
}

func TestRejectsAndIgnores(t *testing.T) {
	good := push("acme/app", "refs/heads/main", sha)
	cases := []struct {
		name, event, body, signature string
		wantCode                     int
	}{
		{"bad signature", "push", good, sign("wrong", good), http.StatusForbidden},
		{"missing signature", "push", good, "", http.StatusForbidden},
		{"unknown repo", "push", push("evil/fork", "refs/heads/main", sha), sign(secret, push("evil/fork", "refs/heads/main", sha)), http.StatusForbidden},
		{"branch not on allowlist", "push", push("acme/app", "refs/heads/feature", sha), "", http.StatusOK},
		{"tag push", "push", push("acme/app", "refs/tags/v1", sha), "", http.StatusOK},
		{"branch deleted", "push", push("acme/app", "refs/heads/main", strings.Repeat("0", 40)), "", http.StatusOK},
		{"other event", "pull_request", good, "", http.StatusOK},
		{"ping", "ping", `{"zen":"x","repository":{"full_name":"acme/app"}}`, "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, q := setup(t)
			sig := c.signature
			if sig == "" && c.wantCode != http.StatusForbidden {
				sig = sign(secret, c.body)
			}
			rec := send(h, c.event, "d-1", c.body, sig)
			if rec.Code != c.wantCode {
				t.Errorf("got %d %s, want %d", rec.Code, rec.Body, c.wantCode)
			}
			if len(q.got) != 0 {
				t.Errorf("a build was queued: %+v", q.got)
			}
		})
	}
}

func TestDuplicateDeliveryIsDropped(t *testing.T) {
	h, q := setup(t)
	body := push("acme/app", "refs/heads/main", sha)
	send(h, "push", "same-id", body, sign(secret, body))
	rec := send(h, "push", "same-id", body, sign(secret, body))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if len(q.got) != 1 {
		t.Errorf("queued %d builds, want 1", len(q.got))
	}
}

func TestDisabledRepoIsIgnored(t *testing.T) {
	h, q := setup(t)
	repo, _ := h.Store.GetRepoByName("acme/app")
	repo.Enabled = false
	h.Store.UpdateRepo(repo, repo.Branches)
	body := push("acme/app", "refs/heads/main", sha)
	rec := send(h, "push", "d-1", body, sign(secret, body))
	if rec.Code != http.StatusOK || len(q.got) != 0 {
		t.Errorf("got %d, queued %d", rec.Code, len(q.got))
	}
}

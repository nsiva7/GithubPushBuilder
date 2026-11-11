// Package webhook handles GitHub push events on POST /hooks/github, the only
// endpoint reachable without the admin session.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

// maxBody matches GitHub's own cap on webhook payloads.
const maxBody = 25 << 20

// Enqueuer is the part of the engine the webhook needs.
type Enqueuer interface {
	Enqueue(store.NewBuild) (int64, error)
}

type Handler struct {
	Store *store.Store
	Queue Enqueuer
}

type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadCommit *struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"head_commit"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reply(w, http.StatusMethodNotAllowed, "error", "POST only", 0)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		reply(w, http.StatusRequestEntityTooLarge, "error", "payload too large", 0)
		return
	}

	// GitHub signs the raw body, whichever content type the hook uses.
	payload := body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			reply(w, http.StatusBadRequest, "error", "malformed form body", 0)
			return
		}
		payload = []byte(form.Get("payload"))
	}
	var ev pushEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		reply(w, http.StatusBadRequest, "error", "malformed JSON", 0)
		return
	}

	// The secret is per repo, so the repo has to be read from the still
	// unverified body. Unknown repos get the same answer as a bad signature.
	repo, err := h.Store.GetRepoByName(ev.Repository.FullName)
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: %v", err)
		reply(w, http.StatusInternalServerError, "error", "internal error", 0)
		return
	}
	secret := repo.WebhookSecret
	if !known || secret == "" {
		secret = "no-such-repo" // still do the HMAC work
	}
	if !validSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) || !known || repo.WebhookSecret == "" {
		reply(w, http.StatusForbidden, "error", "unknown repository or bad signature", 0)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		reply(w, http.StatusOK, "pong", "", 0)
		return
	}
	if event != "push" {
		reply(w, http.StatusOK, "ignored", "only push events start builds", 0)
		return
	}
	if !repo.Enabled {
		reply(w, http.StatusOK, "ignored", "repository is disabled", 0)
		return
	}
	branch, isBranch := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !isBranch {
		reply(w, http.StatusOK, "ignored", "not a branch push", 0)
		return
	}
	if ev.Deleted || strings.Trim(ev.After, "0") == "" {
		reply(w, http.StatusOK, "ignored", "branch was deleted", 0)
		return
	}
	if !h.Store.BranchAllowed(repo.ID, branch) {
		reply(w, http.StatusOK, "ignored", "branch is not on the allowlist", 0)
		return
	}
	if !shaRE.MatchString(ev.After) {
		reply(w, http.StatusBadRequest, "error", "bad commit SHA", 0)
		return
	}
	if id := r.Header.Get("X-GitHub-Delivery"); id != "" {
		fresh, err := h.Store.RecordDelivery(id)
		if err != nil {
			log.Printf("webhook: %v", err)
			reply(w, http.StatusInternalServerError, "error", "internal error", 0)
			return
		}
		if !fresh {
			reply(w, http.StatusOK, "duplicate", "delivery already processed", 0)
			return
		}
	}

	nb := store.NewBuild{
		RepoID: repo.ID, Branch: branch, CommitSHA: ev.After,
		Author: ev.Pusher.Name, Trigger: store.TriggerWebhook,
	}
	if ev.HeadCommit != nil {
		nb.CommitMessage, _, _ = strings.Cut(ev.HeadCommit.Message, "\n")
		if ev.HeadCommit.Author.Name != "" {
			nb.Author = ev.HeadCommit.Author.Name
		}
	}
	id, err := h.Queue.Enqueue(nb)
	if err != nil {
		log.Printf("webhook: enqueue: %v", err)
		reply(w, http.StatusInternalServerError, "error", "internal error", 0)
		return
	}
	log.Printf("webhook: %s %s %.7s queued as build #%d", repo.FullName, branch, ev.After, id)
	reply(w, http.StatusAccepted, "queued", "", id)
}

// validSignature checks X-Hub-Signature-256 ("sha256=<hex>") in constant time.
func validSignature(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	got, err := hex.DecodeString(hexSig)
	return hmac.Equal(got, want) && ok && err == nil
}

func reply(w http.ResponseWriter, code int, status, detail string, buildID int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	out := map[string]any{"status": status}
	if detail != "" {
		out["detail"] = detail
	}
	if buildID > 0 {
		out["build_id"] = buildID
	}
	json.NewEncoder(w).Encode(out)
}

package store

import (
	"path/filepath"
	"testing"
)

func open(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.CreateRepo(Repo{FullName: "acme/app", WebhookSecret: "s", Enabled: true}, []string{"main", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

func enqueue(t *testing.T, st *Store, repo int64, branch, trigger string, maxWaiting int) int64 {
	t.Helper()
	id, _, err := st.Enqueue(NewBuild{RepoID: repo, Branch: branch, CommitSHA: "abc", Trigger: trigger}, maxWaiting)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func status(t *testing.T, st *Store, id int64) string {
	t.Helper()
	b, err := st.GetBuild(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Status
}

func TestWaitingLimitSupersedesOldestPush(t *testing.T) {
	st, repo := open(t)
	first := enqueue(t, st, repo, "main", TriggerWebhook, 1)
	manual := enqueue(t, st, repo, "main", TriggerManual, 1)
	other := enqueue(t, st, repo, "dev", TriggerWebhook, 1)
	second := enqueue(t, st, repo, "main", TriggerWebhook, 1)

	if got := status(t, st, first); got != StatusSuperseded {
		t.Errorf("oldest waiting push: got %s, want superseded", got)
	}
	for name, id := range map[string]int64{"manual run": manual, "other branch": other, "newest push": second} {
		if got := status(t, st, id); got != StatusQueued {
			t.Errorf("%s: got %s, want queued", name, got)
		}
	}
}

func TestWaitingLimitAboveOne(t *testing.T) {
	st, repo := open(t)
	a := enqueue(t, st, repo, "main", TriggerWebhook, 2)
	b := enqueue(t, st, repo, "main", TriggerWebhook, 2)
	c := enqueue(t, st, repo, "main", TriggerWebhook, 2)
	if status(t, st, a) != StatusSuperseded || status(t, st, b) != StatusQueued || status(t, st, c) != StatusQueued {
		t.Errorf("got %s %s %s, want superseded queued queued", status(t, st, a), status(t, st, b), status(t, st, c))
	}
}

func TestRunningBuildIsNeverSuperseded(t *testing.T) {
	st, repo := open(t)
	running := enqueue(t, st, repo, "main", TriggerWebhook, 1)
	if b, err := st.ClaimNext(1); err != nil || b == nil || b.ID != running {
		t.Fatalf("claim: %v %v", b, err)
	}
	waiting := enqueue(t, st, repo, "main", TriggerWebhook, 1)
	if status(t, st, running) != StatusRunning || status(t, st, waiting) != StatusQueued {
		t.Errorf("got %s and %s", status(t, st, running), status(t, st, waiting))
	}
}

func TestClaimRespectsParallelLimitAndBranchExclusion(t *testing.T) {
	st, repo := open(t)
	main1 := enqueue(t, st, repo, "main", TriggerManual, 1)
	enqueue(t, st, repo, "main", TriggerManual, 1) // same branch: must wait
	dev := enqueue(t, st, repo, "dev", TriggerManual, 1)

	if b, _ := st.ClaimNext(1); b == nil || b.ID != main1 {
		t.Fatalf("first claim: %v", b)
	}
	if b, _ := st.ClaimNext(1); b != nil {
		t.Fatalf("limit of 1 exceeded: claimed #%d", b.ID)
	}
	if b, _ := st.ClaimNext(3); b == nil || b.ID != dev {
		t.Fatalf("expected the other branch to be claimed, got %v", b)
	}
	if b, _ := st.ClaimNext(3); b != nil {
		t.Fatalf("two builds of one branch at once: claimed #%d", b.ID)
	}
}

func TestRequeueKeepsQueueOrder(t *testing.T) {
	st, repo := open(t)
	first := enqueue(t, st, repo, "main", TriggerManual, 1)
	enqueue(t, st, repo, "dev", TriggerManual, 1)
	st.ClaimNext(1)
	if n, err := st.RequeueRunning(); err != nil || n != 1 {
		t.Fatalf("requeue: %d %v", n, err)
	}
	if b, _ := st.ClaimNext(1); b == nil || b.ID != first {
		t.Fatalf("re-queued build should run first, got %v", b)
	}
}

func TestSecretsScope(t *testing.T) {
	st, repo := open(t)
	other, _ := st.CreateRepo(Repo{FullName: "acme/other", WebhookSecret: "s"}, []string{"main"})
	st.CreateSecret("SHARED", "all", true, nil)
	st.CreateSecret("TOKEN", "global", true, nil)
	st.CreateSecret("TOKEN", "own", false, []int64{repo})
	st.CreateSecret("ONLY_OTHER", "x", false, []int64{other})

	got, err := st.SecretsForRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got["SHARED"] != "all" || got["TOKEN"] != "own" {
		t.Errorf("got %v", got)
	}
	if _, leaked := got["ONLY_OTHER"]; leaked {
		t.Error("a secret scoped to another repo was returned")
	}
}

func TestDeliveryDedup(t *testing.T) {
	st, _ := open(t)
	if fresh, _ := st.RecordDelivery("d1"); !fresh {
		t.Error("first delivery reported as duplicate")
	}
	if fresh, _ := st.RecordDelivery("d1"); fresh {
		t.Error("duplicate delivery reported as new")
	}
}

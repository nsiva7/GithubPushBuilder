package store

import (
	"database/sql"
	"errors"
	"strconv"
)

// Build states.
const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusSuccess    = "success"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusTimedOut   = "timed_out"
	StatusSuperseded = "superseded"
)

// What started a build.
const (
	TriggerWebhook  = "webhook"  // GitHub push
	TriggerRecovery = "recovery" // push found on startup that no webhook announced
	TriggerManual   = "manual"   // "Run" in the dashboard
	TriggerRetry    = "retry"    // "Retry" in the dashboard
)

type Build struct {
	ID              int64
	RepoID          int64
	Repo            string // owner/name
	Branch          string
	CommitSHA       string // empty until checkout for a manual run of the branch head
	CommitMessage   string
	Author          string
	Trigger         string
	Status          string
	Message         string // why it failed, or what superseded it
	RetryOf         int64
	ArtifactsPruned bool
	CreatedAt       int64
	StartedAt       int64
	FinishedAt      int64
}

// Active reports whether the build is still waiting or running.
func (b Build) Active() bool { return b.Status == StatusQueued || b.Status == StatusRunning }

const buildCols = `b.id, b.repo_id, r.full_name, b.branch, b.commit_sha, b.commit_message, b.author,
	b.triggered_by, b.status, b.message, b.retry_of, b.artifacts_pruned, b.created_at, b.started_at, b.finished_at`

const buildFrom = ` FROM builds b JOIN repos r ON r.id = b.repo_id `

func scanBuild(row interface{ Scan(...any) error }) (Build, error) {
	var b Build
	err := row.Scan(&b.ID, &b.RepoID, &b.Repo, &b.Branch, &b.CommitSHA, &b.CommitMessage, &b.Author,
		&b.Trigger, &b.Status, &b.Message, &b.RetryOf, &b.ArtifactsPruned, &b.CreatedAt, &b.StartedAt, &b.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

func (s *Store) queryBuilds(query string, args ...any) ([]Build, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// NewBuild describes a build to enqueue.
type NewBuild struct {
	RepoID        int64
	Branch        string
	CommitSHA     string
	CommitMessage string
	Author        string
	Trigger       string
	RetryOf       int64
}

// Enqueue adds a build to the queue. Pushes (webhook and recovery) are subject
// to maxWaiting per repo + branch: when the limit is reached the oldest waiting
// pushes are marked superseded. Manual runs and retries are exempt both ways.
func (s *Store) Enqueue(nb NewBuild, maxWaiting int) (id int64, superseded []int64, err error) {
	if maxWaiting < 1 {
		maxWaiting = 1
	}
	limited := nb.Trigger == TriggerWebhook || nb.Trigger == TriggerRecovery
	err = s.tx(func(tx *sql.Tx) error {
		if limited {
			rows, err := tx.Query(`SELECT id FROM builds WHERE repo_id = ? AND branch = ? AND status = ?
				AND triggered_by IN (?, ?) ORDER BY id`,
				nb.RepoID, nb.Branch, StatusQueued, TriggerWebhook, TriggerRecovery)
			if err != nil {
				return err
			}
			var waiting []int64
			for rows.Next() {
				var w int64
				if err := rows.Scan(&w); err != nil {
					rows.Close()
					return err
				}
				waiting = append(waiting, w)
			}
			rows.Close()
			for len(waiting) >= maxWaiting {
				superseded = append(superseded, waiting[0])
				waiting = waiting[1:]
			}
		}
		res, err := tx.Exec(`INSERT INTO builds (repo_id, branch, commit_sha, commit_message, author,
			triggered_by, status, retry_of, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nb.RepoID, nb.Branch, nb.CommitSHA, nb.CommitMessage, nb.Author, nb.Trigger, StatusQueued, nb.RetryOf, now())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		for _, old := range superseded {
			if _, err := tx.Exec(`UPDATE builds SET status = ?, finished_at = ?, message = ? WHERE id = ?`,
				StatusSuperseded, now(), "Superseded by a newer push (build #"+strconv.FormatInt(id, 10)+")", old); err != nil {
				return err
			}
		}
		return nil
	})
	return id, superseded, err
}

// ClaimNext moves the oldest runnable queued build to running and returns it.
// It returns nil when maxParallel builds are already running or nothing is
// runnable. Two builds of the same repo + branch never run together.
func (s *Store) ClaimNext(maxParallel int) (*Build, error) {
	var claimed *Build
	err := s.tx(func(tx *sql.Tx) error {
		var running int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM builds WHERE status = ?`, StatusRunning).Scan(&running); err != nil {
			return err
		}
		if running >= maxParallel {
			return nil
		}
		var id int64
		err := tx.QueryRow(`SELECT q.id FROM builds q WHERE q.status = ?
			AND NOT EXISTS (SELECT 1 FROM builds x WHERE x.status = ?
				AND x.repo_id = q.repo_id AND x.branch = q.branch)
			ORDER BY q.id LIMIT 1`, StatusQueued, StatusRunning).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE builds SET status = ?, started_at = ?, finished_at = 0, message = '' WHERE id = ?`,
			StatusRunning, now(), id); err != nil {
			return err
		}
		b, err := scanBuild(tx.QueryRow(`SELECT `+buildCols+buildFrom+`WHERE b.id = ?`, id))
		if err != nil {
			return err
		}
		claimed = &b
		return nil
	})
	return claimed, err
}

// FinishBuild records the final state of a running build.
func (s *Store) FinishBuild(id int64, status, message string) error {
	_, err := s.db.Exec(`UPDATE builds SET status = ?, message = ?, finished_at = ? WHERE id = ?`,
		status, message, now(), id)
	return err
}

// CancelQueued cancels a build that has not started. It reports whether it did.
func (s *Store) CancelQueued(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE builds SET status = ?, message = ?, finished_at = ? WHERE id = ? AND status = ?`,
		StatusCancelled, "Cancelled before it started", now(), id, StatusQueued)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RequeueRunning puts builds that were running when the service stopped back
// in the queue. They keep their IDs, so they stay at the front.
func (s *Store) RequeueRunning() (int64, error) {
	res, err := s.db.Exec(`UPDATE builds SET status = ?, started_at = 0 WHERE status = ?`,
		StatusQueued, StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetBuildCommit fills in the commit once checkout has resolved it.
func (s *Store) SetBuildCommit(id int64, sha, message, author string) error {
	_, err := s.db.Exec(`UPDATE builds SET commit_sha = ?, commit_message = ?, author = ? WHERE id = ?`,
		sha, message, author, id)
	return err
}

func (s *Store) GetBuild(id int64) (Build, error) {
	return scanBuild(s.db.QueryRow(`SELECT `+buildCols+buildFrom+`WHERE b.id = ?`, id))
}

func (s *Store) BuildsByStatus(status string) ([]Build, error) {
	return s.queryBuilds(`SELECT `+buildCols+buildFrom+`WHERE b.status = ? ORDER BY b.id`, status)
}

// History returns finished builds, newest first. repoID 0 means every repo.
func (s *Store) History(repoID int64, limit, offset int) ([]Build, int, error) {
	where := `WHERE b.status NOT IN (?, ?)`
	args := []any{StatusQueued, StatusRunning}
	if repoID > 0 {
		where += ` AND b.repo_id = ?`
		args = append(args, repoID)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*)`+buildFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	builds, err := s.queryBuilds(`SELECT `+buildCols+buildFrom+where+` ORDER BY b.id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	return builds, total, err
}

// ActiveCount returns how many builds of a repo are queued or running.
func (s *Store) ActiveCount(repoID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE repo_id = ? AND status IN (?, ?)`,
		repoID, StatusQueued, StatusRunning).Scan(&n)
	return n, err
}

// BuildIDs returns every build ID of a repo (used to remove its log files).
func (s *Store) BuildIDs(repoID int64) ([]int64, error) {
	return s.queryIDs(`SELECT id FROM builds WHERE repo_id = ?`, repoID)
}

// BranchHasBuilds reports whether the branch was ever built or queued.
func (s *Store) BranchHasBuilds(repoID int64, branch string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE repo_id = ? AND branch = ?`, repoID, branch).Scan(&n)
	return n > 0
}

// CommitCovered reports whether the commit on the branch was built or is going
// to be: a build exists for it, or one is waiting to build the branch head.
func (s *Store) CommitCovered(repoID int64, branch, sha string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE repo_id = ? AND branch = ?
		AND (commit_sha = ? OR (commit_sha = '' AND status IN (?, ?)))`,
		repoID, branch, sha, StatusQueued, StatusRunning).Scan(&n)
	return n > 0
}

// ---- artifacts ----

type Artifact struct {
	ID      int64
	BuildID int64
	Path    string // relative to the build's artifact directory, slash-separated
	Size    int64
	SHA256  string
}

func (s *Store) AddArtifact(buildID int64, path string, size int64, sha256 string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO artifacts (build_id, path, size, sha256) VALUES (?, ?, ?, ?)`,
		buildID, path, size, sha256)
	return err
}

func (s *Store) ListArtifacts(buildID int64) ([]Artifact, error) {
	rows, err := s.db.Query(`SELECT id, build_id, path, size, sha256 FROM artifacts WHERE build_id = ? ORDER BY path`, buildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.BuildID, &a.Path, &a.Size, &a.SHA256); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetArtifact(buildID int64, path string) (Artifact, error) {
	var a Artifact
	err := s.db.QueryRow(`SELECT id, build_id, path, size, sha256 FROM artifacts WHERE build_id = ? AND path = ?`,
		buildID, path).Scan(&a.ID, &a.BuildID, &a.Path, &a.Size, &a.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ClearArtifacts drops the artifact rows of a build before it is (re)indexed.
func (s *Store) ClearArtifacts(buildID int64) error {
	_, err := s.db.Exec(`DELETE FROM artifacts WHERE build_id = ?`, buildID)
	return err
}

// PrunableBuilds returns builds of repo + branch whose artifacts fall outside
// the newest keep builds that actually ran.
func (s *Store) PrunableBuilds(repoID int64, branch string, keep int) ([]int64, error) {
	return s.queryIDs(`SELECT id FROM builds WHERE repo_id = ? AND branch = ? AND artifacts_pruned = 0
		AND status NOT IN (?, ?) AND id NOT IN (
			SELECT id FROM builds WHERE repo_id = ? AND branch = ? AND started_at > 0
			ORDER BY id DESC LIMIT ?)`,
		repoID, branch, StatusQueued, StatusRunning, repoID, branch, keep)
}

// MarkArtifactsPruned records that a build's artifact files were deleted.
func (s *Store) MarkArtifactsPruned(buildID int64) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM artifacts WHERE build_id = ?`, buildID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE builds SET artifacts_pruned = 1 WHERE id = ?`, buildID)
		return err
	})
}

func (s *Store) queryIDs(query string, args ...any) ([]int64, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

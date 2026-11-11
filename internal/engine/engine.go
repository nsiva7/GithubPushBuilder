// Package engine owns the build queue workers: it claims queued builds, runs
// checkout -> pre script -> repo script -> post script, and records the result.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsiva7/GithubPushBuilder/internal/config"
	"github.com/nsiva7/GithubPushBuilder/internal/defaults"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

var ErrNotActive = errors.New("build is not queued or running")

type Engine struct {
	cfg  config.Config
	st   *store.Store
	wake chan struct{}
	ctx  context.Context

	mu     sync.Mutex // guards active, and makes claim + register atomic against Cancel
	active map[int64]*activeBuild

	repoMu sync.Map // repo ID -> *sync.Mutex, serialises fetches into the git cache
	wg     sync.WaitGroup
}

type activeBuild struct {
	ctx        context.Context
	cancel     context.CancelFunc
	userCancel atomic.Bool
}

func New(cfg config.Config, st *store.Store) *Engine {
	return &Engine{cfg: cfg, st: st, wake: make(chan struct{}, 1), active: map[int64]*activeBuild{}}
}

// Start prepares the data directory, recovers from a previous run and starts
// the scheduler. Cancelling ctx stops running builds; they are re-queued on
// the next start.
func (e *Engine) Start(ctx context.Context) error {
	e.ctx = ctx
	for _, d := range []string{"scripts", "workspaces", "logs", "artifacts", "cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(e.cfg.DataDir, d), 0o755); err != nil {
			return err
		}
	}
	if err := defaults.Install(filepath.Join(e.cfg.DataDir, "scripts")); err != nil {
		return fmt.Errorf("install default scripts: %w", err)
	}

	// Nothing is running yet, so anything left in these is from a crashed run.
	for _, d := range []string{"workspaces", "tmp"} {
		entries, _ := os.ReadDir(filepath.Join(e.cfg.DataDir, d))
		for _, ent := range entries {
			forceRemoveAll(filepath.Join(e.cfg.DataDir, d, ent.Name()))
		}
	}
	n, err := e.st.RequeueRunning()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("recovery: re-queued %d build(s) that were running at shutdown", n)
	}

	e.wg.Add(2)
	go e.scheduler()
	go func() {
		defer e.wg.Done()
		e.catchUpMissedPushes()
	}()
	return nil
}

// Wait blocks until the scheduler and all build goroutines have stopped.
func (e *Engine) Wait() { e.wg.Wait() }

// Wake asks the scheduler to look at the queue now.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) scheduler() {
	defer e.wg.Done()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	housekeeping := time.NewTicker(time.Hour)
	defer housekeeping.Stop()
	for {
		e.dispatch()
		select {
		case <-e.ctx.Done():
			return
		case <-e.wake:
		case <-tick.C:
		case <-housekeeping.C:
			e.st.PruneDeliveries(14 * 24 * time.Hour)
			e.st.PruneSessions()
		}
	}
}

// dispatch starts queued builds until the parallel limit is reached. The limit
// is read each time, so a changed setting applies from the next build.
func (e *Engine) dispatch() {
	for e.ctx.Err() == nil {
		e.mu.Lock()
		b, err := e.st.ClaimNext(e.st.SettingInt(store.KeyMaxParallel))
		if err != nil || b == nil {
			e.mu.Unlock()
			if err != nil {
				log.Printf("queue: %v", err)
			}
			return
		}
		ctx, cancel := context.WithCancel(e.ctx)
		ab := &activeBuild{ctx: ctx, cancel: cancel}
		e.active[b.ID] = ab
		e.mu.Unlock()

		e.wg.Add(1)
		go func(b store.Build) {
			defer e.wg.Done()
			defer func() {
				if p := recover(); p != nil {
					log.Printf("build #%d panicked: %v", b.ID, p)
					e.st.FinishBuild(b.ID, store.StatusFailed, fmt.Sprintf("Internal error: %v", p))
				}
				cancel()
				e.mu.Lock()
				delete(e.active, b.ID)
				e.mu.Unlock()
				e.Wake()
			}()
			e.execute(ab, b)
		}(*b)
	}
}

// Enqueue adds a build and wakes the scheduler.
func (e *Engine) Enqueue(nb store.NewBuild) (int64, error) {
	id, superseded, err := e.st.Enqueue(nb, e.st.SettingInt(store.KeyMaxWaiting))
	if err != nil {
		return 0, err
	}
	for _, old := range superseded {
		log.Printf("build #%d superseded by #%d", old, id)
	}
	e.Wake()
	return id, nil
}

// Cancel stops a queued or running build.
func (e *Engine) Cancel(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ok, err := e.st.CancelQueued(id)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	ab, running := e.active[id]
	if !running {
		return ErrNotActive
	}
	ab.userCancel.Store(true)
	ab.cancel()
	return nil
}

// BaseURL is the public URL of the dashboard, without a trailing slash. It may be empty.
func (e *Engine) BaseURL() string {
	if v := strings.TrimRight(e.st.Setting(store.KeyBaseURL), "/"); v != "" {
		return v
	}
	return e.cfg.BaseURL
}

// ---- paths on the data volume ----

func (e *Engine) LogPath(id int64) string {
	return filepath.Join(e.cfg.DataDir, "logs", strconv.FormatInt(id, 10)+".log")
}

func (e *Engine) ArtifactDir(repoFullName string, id int64) string {
	return filepath.Join(e.cfg.DataDir, "artifacts", filepath.FromSlash(repoFullName), strconv.FormatInt(id, 10))
}

func (e *Engine) workspaceDir(id int64) string {
	return filepath.Join(e.cfg.DataDir, "workspaces", strconv.FormatInt(id, 10))
}

func (e *Engine) tmpDir(id int64) string {
	return filepath.Join(e.cfg.DataDir, "tmp", strconv.FormatInt(id, 10))
}

func (e *Engine) cacheDir() string { return filepath.Join(e.cfg.DataDir, "cache") }

// scriptPath returns the pre.sh or post.sh to use for a repo: the per-repo
// override if present, else the global one, else "".
func (e *Engine) scriptPath(repo store.Repo, name string) string {
	for _, p := range []string{
		filepath.Join(e.cfg.DataDir, "scripts", repo.Owner(), repo.Name(), name),
		filepath.Join(e.cfg.DataDir, "scripts", name),
	} {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// RemoveRepoFiles deletes what a repo left on disk: logs, artifacts, git cache.
// Call it after the repo's builds are gone from the database.
func (e *Engine) RemoveRepoFiles(repo store.Repo, buildIDs []int64) {
	for _, id := range buildIDs {
		os.Remove(e.LogPath(id))
	}
	forceRemoveAll(filepath.Join(e.cfg.DataDir, "artifacts", filepath.FromSlash(repo.FullName)))
	forceRemoveAll(e.mirrorDir(repo))
}

// prune deletes the artifacts of builds beyond the retention limit.
func (e *Engine) prune(repo store.Repo, branch string) {
	ids, err := e.st.PrunableBuilds(repo.ID, branch, e.st.SettingInt(store.KeyRetention))
	if err != nil {
		log.Printf("retention: %v", err)
		return
	}
	for _, id := range ids {
		forceRemoveAll(e.ArtifactDir(repo.FullName, id))
		if err := e.st.MarkArtifactsPruned(id); err != nil {
			log.Printf("retention: build #%d: %v", id, err)
		}
	}
}

// catchUpMissedPushes compares the head of every allowlisted branch with the
// commits already built and queues a build for heads that no webhook announced.
// Branches that were never built are left alone: there is nothing to compare
// with, and registering a repo should not start builds on the next restart.
func (e *Engine) catchUpMissedPushes() {
	repos, err := e.st.ListRepos()
	if err != nil {
		log.Printf("recovery: %v", err)
		return
	}
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		for _, branch := range repo.Branches {
			if e.ctx.Err() != nil {
				return
			}
			if !e.st.BranchHasBuilds(repo.ID, branch) {
				continue
			}
			ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
			sha, err := e.remoteHead(ctx, repo, branch)
			cancel()
			if err != nil {
				log.Printf("recovery: %s %s: cannot read branch head: %v", repo.FullName, branch, err)
				continue
			}
			if sha == "" || e.st.CommitCovered(repo.ID, branch, sha) {
				continue
			}
			id, err := e.Enqueue(store.NewBuild{
				RepoID: repo.ID, Branch: branch, CommitSHA: sha, Trigger: store.TriggerRecovery,
			})
			if err != nil {
				log.Printf("recovery: %s %s: %v", repo.FullName, branch, err)
				continue
			}
			log.Printf("recovery: %s %s moved to %.7s while the service was down; queued build #%d",
				repo.FullName, branch, sha, id)
		}
	}
}

// forceRemoveAll is os.RemoveAll that also copes with read-only directories,
// which build tools like to leave behind.
func forceRemoveAll(path string) {
	if os.RemoveAll(path) == nil {
		return
	}
	filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
	if err := os.RemoveAll(path); err != nil {
		log.Printf("cleanup: %v", err)
	}
}

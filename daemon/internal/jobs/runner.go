// Package jobs implements the FIFO job queue. It is resident in `jarjard serve`
// and spawns a separate `jarjard worker --job <id>` process per job for memory
// isolation (ARCHITECTURE.md §4). Concurrency is 1: never two workers.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/akimbohh/jarjar/daemon/internal/api"
	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// runnableStatuses are the states for which the runner should spawn a worker.
var runnableStatuses = map[string]bool{
	store.StatusQueued:        true,
	store.StatusPlanning:      true,
	store.StatusMaterializing: true,
}

type Runner struct {
	cfg        config.Config
	store      *store.Store
	log        *slog.Logger
	selfExe    string
	configPath string

	wake chan struct{}

	mu      sync.Mutex
	current *string
}

// New builds a Runner. selfExe is the path to the jarjard binary; configPath is
// passed to spawned workers.
func New(cfg config.Config, st *store.Store, log *slog.Logger, selfExe, configPath string) *Runner {
	return &Runner{
		cfg:        cfg,
		store:      st,
		log:        log,
		selfExe:    selfExe,
		configPath: configPath,
		wake:       make(chan struct{}, 1),
	}
}

// Wake nudges the runner to check for work (api.Jobs).
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run drives the queue until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	r.Wake() // process anything already queued at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		for {
			did, err := r.processOne(ctx)
			if err != nil {
				r.log.Error("job processing", "err", err)
			}
			if !did || ctx.Err() != nil {
				break
			}
		}
	}
}

// processOne spawns a worker for the active request if it is runnable. Returns
// true if it ran a worker (caller loops to pick up the next state).
func (r *Runner) processOne(ctx context.Context) (bool, error) {
	active, ok, err := r.store.ActiveRequest(ctx)
	if err != nil {
		return false, err
	}
	if !ok || !runnableStatuses[active.Status] {
		return false, nil
	}

	r.setCurrent(&active.ID)
	defer r.setCurrent(nil)

	r.log.Info("spawning worker", "req", active.ID, "status", active.Status)
	if err := r.spawnWorker(ctx, active.ID); err != nil {
		// Spawn/exec failure: mark the request failed so it doesn't wedge the queue.
		r.log.Error("worker spawn failed", "req", active.ID, "err", err)
		r.store.SetStatus(ctx, active.ID, store.StatusFailed, store.WithError("internal: worker failed to start"))
		return true, nil
	}

	// Verify the worker actually advanced the request; otherwise it crashed.
	after, aerr := r.store.RequestByID(ctx, active.ID)
	if aerr == nil && runnableStatuses[after.Status] {
		r.log.Error("worker exited without advancing request", "req", active.ID, "status", after.Status)
		r.store.SetStatus(ctx, active.ID, store.StatusFailed, store.WithError("internal: worker exited unexpectedly"))
	}
	return true, nil
}

func (r *Runner) spawnWorker(ctx context.Context, jobID string) error {
	args := []string{"worker", "--job", jobID, "--config", r.configPath}
	name := r.selfExe

	// Wrap in a memory-capped transient scope when systemd-run is available.
	if path, err := exec.LookPath("systemd-run"); err == nil {
		scoped := []string{
			"--scope", "--quiet",
			"-p", "MemoryMax=" + r.cfg.Claude.MemoryMax,
			"-p", "MemorySwapMax=2G",
			r.selfExe,
		}
		args = append(scoped, args...)
		name = path
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// EnqueueRollback creates a synthetic rollback request (api.Jobs).
func (r *Runner) EnqueueRollback(ctx context.Context, toVersion int, playerID string) (string, error) {
	req, err := r.store.CreateRequest(ctx, playerID, fmt.Sprintf("Roll back to version %d", toVersion))
	if err != nil {
		return "", err
	}
	planJSON, _ := json.Marshal(map[string]int{"rollback_to": toVersion})
	if _, err := r.store.SetStatus(ctx, req.ID, store.StatusQueued, store.WithPlan(string(planJSON))); err != nil {
		return "", err
	}
	return req.ID, nil
}

// Snapshot returns queue state (api.Jobs).
func (r *Runner) Snapshot() api.JobsSnapshot {
	ctx := context.Background()
	reqs, _ := r.store.ListRequests(ctx, 100, "")
	var queue []string
	for _, req := range reqs {
		if !store.IsTerminal(req.Status) {
			queue = append(queue, req.ID)
		}
	}
	r.mu.Lock()
	cur := r.current
	r.mu.Unlock()
	return api.JobsSnapshot{Queue: queue, CurrentJob: cur}
}

func (r *Runner) setCurrent(cur *string) {
	r.mu.Lock()
	r.current = cur
	r.mu.Unlock()
}

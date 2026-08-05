// Package worker implements the request → published-version pipeline
// (PIPELINE.md). It runs as a separate `jarjard worker --job <id>` process for
// memory isolation from the resident daemon. Stages: checkout → plan (Claude
// Code) → validate → materialize → build → server-apply → publish.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/mcserver"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

const maxClarificationRounds = 2

// Deps are the collaborators a worker run needs.
type Deps struct {
	Cfg      config.Config
	Store    *store.Store
	Pack     *pack.Pack
	Registry *modtool.Registry
	MC       *mcserver.Controller
	Log      *slog.Logger
}

// Run executes the pipeline for one job (request id). It always updates the
// request to a terminal or waiting status and emits events; it returns an error
// only for unexpected internal failures (the runner logs them).
func Run(ctx context.Context, d Deps, jobID string) error {
	req, err := d.Store.RequestByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load request %s: %w", jobID, err)
	}

	// Rollback jobs carry a {"rollback_to": N} plan.
	if req.PlanJSON != nil {
		var rb rollbackPlan
		if json.Unmarshal([]byte(*req.PlanJSON), &rb) == nil && rb.RollbackTo > 0 {
			return d.runRollback(ctx, req, rb.RollbackTo)
		}
	}

	switch req.Status {
	case store.StatusMaterializing:
		// Approved: resume from the staged worktree.
		return d.resumeAfterApproval(ctx, req)
	case store.StatusQueued, store.StatusPlanning:
		return d.runPlanning(ctx, req)
	default:
		// Nothing to do (waiting/terminal). This is not an error.
		return nil
	}
}

func (d Deps) runPlanning(ctx context.Context, req store.Request) error {
	req = d.transition(ctx, req, store.StatusPlanning)

	worktree, err := d.Pack.AddWorktree(ctx, req.ID)
	if err != nil {
		return d.fail(ctx, req, "checkout: "+err.Error())
	}
	defer d.Pack.RemoveWorktree(ctx, req.ID)

	if err := d.writeClaudeMD(ctx, worktree); err != nil {
		return d.fail(ctx, req, "prepare worktree: "+err.Error())
	}

	plan, err := d.plan(ctx, worktree, req)
	if err != nil {
		return d.fail(ctx, req, "planning: "+err.Error())
	}

	switch plan.Status {
	case PlanNeedsClarification:
		if req.ClarificationRounds >= maxClarificationRounds {
			return d.fail(ctx, req, "too many clarification rounds; please rephrase your request")
		}
		q := ""
		if plan.ClarificationQuestion != nil {
			q = *plan.ClarificationQuestion
		}
		updated, err := d.Store.SetStatus(ctx, req.ID, store.StatusAwaitingClarification, store.WithQuestion(q))
		if err != nil {
			return err
		}
		d.emit(ctx, updated)
		return nil
	case PlanInfeasible:
		updated, err := d.Store.SetStatus(ctx, req.ID, store.StatusInfeasible,
			store.WithPlan(mustJSON(plan)), store.WithError(plan.Summary))
		if err != nil {
			return err
		}
		d.emit(ctx, updated)
		return nil
	}

	// Validate (V1–V9).
	vr, err := d.validate(ctx, worktree, plan)
	if err != nil {
		return d.fail(ctx, req, "validation: "+err.Error())
	}
	plan = vr.Plan // may carry auto-added dependency actions

	// Approval gate (V9).
	needApproval := d.Cfg.Pipeline.RequireApproval || plan.Confidence == ConfLow
	if needApproval {
		if _, err := d.Pack.StagePending(ctx, req.ID, worktree, "req "+req.ID+": staged for approval"); err != nil {
			return d.fail(ctx, req, "stage for approval: "+err.Error())
		}
		updated, err := d.Store.SetStatus(ctx, req.ID, store.StatusAwaitingApproval, store.WithPlan(mustJSON(plan)))
		if err != nil {
			return err
		}
		d.emit(ctx, updated)
		return nil
	}

	// No approval needed: continue straight through in this worktree.
	req = d.transition(ctx, d.setPlan(ctx, req, plan), store.StatusMaterializing)
	return d.finish(ctx, req, worktree, plan)
}

func (d Deps) resumeAfterApproval(ctx context.Context, req store.Request) error {
	if req.PlanJSON == nil {
		return d.fail(ctx, req, "resume: no stored plan")
	}
	var plan Plan
	if err := json.Unmarshal([]byte(*req.PlanJSON), &plan); err != nil {
		return d.fail(ctx, req, "resume: bad stored plan: "+err.Error())
	}
	worktree, err := d.Pack.AddWorktreeFromPending(ctx, req.ID)
	if err != nil {
		return d.fail(ctx, req, "resume checkout: "+err.Error())
	}
	defer func() {
		d.Pack.RemoveWorktree(ctx, req.ID)
		d.Pack.DropPending(ctx, req.ID)
	}()
	return d.finish(ctx, req, worktree, plan)
}

// finish runs materialize → build → server-apply → publish.
func (d Deps) finish(ctx context.Context, req store.Request, worktree string, plan Plan) error {
	// Materialize (deterministic, no AI).
	if err := d.materialize(ctx, worktree, plan); err != nil {
		return d.fail(ctx, req, "materialize: "+err.Error())
	}

	// Determine the new version number.
	number := 1
	if cur, ok, err := d.Store.CurrentVersion(ctx); err != nil {
		return d.fail(ctx, req, "version lookup: "+err.Error())
	} else if ok {
		number = cur.Number + 1
	}

	changelog := d.buildChangelog(plan)
	reqID := req.ID
	m, commit, err := d.Pack.BuildVersion(ctx, pack.BuildInput{
		Worktree:  worktree,
		Number:    number,
		RequestID: &reqID,
		Summary:   plan.Summary,
		Changelog: changelog,
		CommitMsg: fmt.Sprintf("req %s: %s", req.ID, firstLine(plan.Summary)),
		Now:       time.Now(),
	})
	if err != nil {
		return d.fail(ctx, req, "build: "+err.Error())
	}

	// Server apply (stage 6/7).
	req = d.transition(ctx, req, store.StatusApplyingServer)
	if err := d.serverApply(ctx, number, m, plan); err != nil {
		// serverApply restored the server files/boot; now undo the pack build so
		// the repo and manifests stay consistent with the last good version.
		if number > 1 {
			if rerr := d.Pack.ResetToTag(ctx, "v"+itoa(number-1)); rerr != nil {
				d.Log.Error("pack rollback after failed apply", "err", rerr)
			}
		}
		d.Pack.DeleteTag(ctx, "v"+itoa(number))
		d.Pack.DeleteManifest(number)
		return d.fail(ctx, req, "server apply: "+err.Error())
	}

	// Publish (atomic: version row exists).
	if err := d.Store.CreateVersion(ctx, store.Version{
		Number:       number,
		CreatedAt:    time.Now().UTC(),
		RequestID:    &reqID,
		GitCommit:    commit,
		Summary:      plan.Summary,
		Status:       "published",
		ManifestPath: d.Cfg.Server.DataDir + "/manifests/" + itoa(number) + ".json",
	}); err != nil {
		return d.fail(ctx, req, "publish: "+err.Error())
	}
	updated, err := d.Store.SetStatus(ctx, req.ID, store.StatusPublished, store.WithVersion(number))
	if err != nil {
		return err
	}
	d.emit(ctx, updated)
	d.emitVersionPublished(ctx, number, plan.Summary)

	// Refresh retained set + GC (best effort).
	if err := d.Pack.RefreshRetained(ctx, d.Store, 10); err != nil {
		d.Log.Warn("refresh retained", "err", err)
	}
	return nil
}

// ---- small helpers ----

func (d Deps) transition(ctx context.Context, req store.Request, to string) store.Request {
	if req.Status == to {
		return req
	}
	updated, err := d.Store.SetStatus(ctx, req.ID, to)
	if err != nil {
		d.Log.Warn("transition", "req", req.ID, "to", to, "err", err)
		return req
	}
	d.emit(ctx, updated)
	return updated
}

func (d Deps) setPlan(ctx context.Context, req store.Request, plan Plan) store.Request {
	updated, err := d.Store.SetStatus(ctx, req.ID, req.Status, store.WithPlan(mustJSON(plan)))
	if err != nil {
		return req
	}
	return updated
}

func (d Deps) fail(ctx context.Context, req store.Request, msg string) error {
	d.Log.Error("job failed", "req", req.ID, "reason", msg)
	updated, err := d.Store.SetStatus(ctx, req.ID, store.StatusFailed, store.WithError(msg))
	if err != nil {
		// If the transition itself is illegal (already terminal), just log.
		d.Log.Warn("mark failed", "req", req.ID, "err", err)
		return nil
	}
	d.emit(ctx, updated)
	return nil
}

func (d Deps) emit(ctx context.Context, r store.Request) {
	payload := map[string]any{
		"request_id":  r.ID,
		"status":      r.Status,
		"player_name": r.PlayerName,
		"text":        r.Text,
		"question":    r.ClarificationQuestion,
		"error":       r.Error,
		"version":     r.Version,
	}
	buf, _ := json.Marshal(payload)
	d.Store.AppendEvent(ctx, store.EventRequestUpdated, buf)
}

func (d Deps) emitVersionPublished(ctx context.Context, version int, summary string) {
	buf, _ := json.Marshal(map[string]any{"version": version, "summary": summary})
	d.Store.AppendEvent(ctx, store.EventVersionPublished, buf)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	if len(s) > 72 {
		return s[:72]
	}
	return s
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

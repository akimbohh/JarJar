package worker

import (
	"context"
	"fmt"

	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// runRollback publishes a new version whose content equals an older version
// (PIPELINE.md §9), then applies it to the server. Version numbers never
// decrease — a rollback is a new forward version.
func (d Deps) runRollback(ctx context.Context, req store.Request, toVersion int) error {
	req = d.transition(ctx, req, store.StatusMaterializing)

	worktree, err := d.Pack.AddWorktreeFromTag(ctx, req.ID, fmt.Sprintf("v%d", toVersion))
	if err != nil {
		return d.fail(ctx, req, "rollback checkout: "+err.Error())
	}
	defer d.Pack.RemoveWorktree(ctx, req.ID)

	// An empty plan: the worktree already holds the target version's exact tree
	// (configs + regenerated lock). finish() re-materializes (no-op), rebuilds
	// the manifest, applies to the server, and publishes.
	plan := Plan{
		SchemaVersion: 1,
		Status:        PlanOK,
		Actions:       []Action{},
		Summary:       fmt.Sprintf("Rolled back to version %d.", toVersion),
		AdminNotes:    fmt.Sprintf("Rollback to v%d requested by admin.", toVersion),
		Confidence:    ConfHigh,
	}
	return d.finish(ctx, req, worktree, plan)
}

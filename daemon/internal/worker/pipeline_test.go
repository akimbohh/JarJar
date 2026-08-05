package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// fakeClaude returns the absolute path to testdata/fake-claude.sh, which stands
// in for Claude Code in CI (no token, no network). See PIPELINE.md §4.
func fakeClaude(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-claude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("fake-claude script not found: %v", err)
	}
	return p
}

// setupPack initializes a v1 pack repo (git) with meta, empty lock, and a
// server dir carrying server.properties.
func setupPack(t *testing.T, dataDir, serverDir string) *pack.Pack {
	t.Helper()
	ctx := context.Background()
	pk := pack.New(dataDir)
	repo := pk.RepoDir()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pack.NewGit(repo).Init(ctx); err != nil {
		t.Fatalf("git init: %v", err)
	}
	meta := pack.PackMeta{SchemaVersion: 1, Name: "T", MCVersion: "1.21.1",
		Loader: pack.Loader{ID: "fabric", Version: "0.16.0"}, SideOverrides: map[string]string{}}
	if err := pack.WriteMetaTo(repo, meta); err != nil {
		t.Fatal(err)
	}
	if err := pack.WriteLockTo(repo, pack.ModsLock{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	// Commit as version 1 baseline.
	if _, _, err := pk.BuildVersion(ctx, pack.BuildInput{
		Worktree: repo, Number: 1, Summary: "init",
		Changelog: []pack.ChangelogEntry{{Kind: "other", Text: "init"}},
		CommitMsg: "v1", Now: time.Now(),
	}); err != nil {
		t.Fatalf("build v1: %v", err)
	}
	os.MkdirAll(serverDir, 0o755)
	os.WriteFile(filepath.Join(serverDir, "server.properties"), []byte("view-distance=10\n"), 0o644)
	return pk
}

// TestPlanAndValidateConfigOnly exercises checkout → plan (fake claude) →
// validate for a config-only change, with no registry or server needed.
func TestPlanAndValidateConfigOnly(t *testing.T) {
	dataDir := t.TempDir()
	serverDir := t.TempDir()
	pk := setupPack(t, dataDir, serverDir)
	ctx := context.Background()

	cfg := config.Default()
	cfg.Server.DataDir = dataDir
	cfg.Minecraft.ServerDir = serverDir
	cfg.Claude.Binary = fakeClaude(t)
	cfg.Claude.TimeoutMinutes = 2

	d := Deps{
		Cfg:      cfg,
		Pack:     pk,
		Registry: modtool.New(false, "", "test"),
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	worktree, err := pk.AddWorktree(ctx, "job-test")
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	defer pk.RemoveWorktree(ctx, "job-test")

	if err := d.writeClaudeMD(ctx, worktree); err != nil {
		t.Fatalf("writeClaudeMD: %v", err)
	}

	req := store.Request{ID: "req_test", PlayerName: "alice", Text: "set a test value"}
	plan, err := d.plan(ctx, worktree, req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Status != PlanOK || len(plan.Actions) != 1 || plan.Actions[0].Type != ActConfigEdit {
		t.Fatalf("unexpected plan: %+v", plan)
	}

	vr, err := d.validate(ctx, worktree, plan)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(vr.Adds) != 0 || len(vr.Removes) != 0 {
		t.Fatalf("config-only change should add/remove no mods: %+v", vr)
	}
	// The fake edit must be on disk.
	if _, err := os.Stat(filepath.Join(worktree, "config", "test.toml")); err != nil {
		t.Fatalf("expected config/test.toml: %v", err)
	}
}

// TestValidateRejectsDisallowedPath ensures V2 rejects edits outside the allowed
// dirs, and V3 rejects a diff/plan mismatch.
func TestValidateRejectsMismatch(t *testing.T) {
	dataDir := t.TempDir()
	serverDir := t.TempDir()
	pk := setupPack(t, dataDir, serverDir)
	ctx := context.Background()

	cfg := config.Default()
	cfg.Server.DataDir = dataDir
	cfg.Minecraft.ServerDir = serverDir
	d := Deps{Cfg: cfg, Pack: pk, Registry: modtool.New(false, "", "t"),
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}

	worktree, _ := pk.AddWorktree(ctx, "job-mism")
	defer pk.RemoveWorktree(ctx, "job-mism")

	// Make a change but record NO config_edit → V3 mismatch.
	os.MkdirAll(filepath.Join(worktree, "config"), 0o755)
	os.WriteFile(filepath.Join(worktree, "config", "x.json"), []byte("{}"), 0o644)
	plan := Plan{SchemaVersion: 1, Status: PlanOK, Actions: []Action{}, Summary: "s", Confidence: ConfHigh}
	if _, err := d.validate(ctx, worktree, plan); err == nil {
		t.Fatal("expected V3 mismatch error")
	}
}

func TestPlanSchemaValidation(t *testing.T) {
	good := Plan{SchemaVersion: 1, Status: "ok", Actions: []Action{}, Summary: "s", AdminNotes: "", Confidence: "high"}
	if err := validatePlanSchema(good); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	// Marshal round-trip sanity.
	if _, err := json.Marshal(good); err != nil {
		t.Fatal(err)
	}
}

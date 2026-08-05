package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMigrateAndPlayers(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	p, err := st.CreatePlayer(ctx, "alice", "tok-alice", RolePlayer)
	if err != nil {
		t.Fatalf("create player: %v", err)
	}
	got, err := st.PlayerByToken(ctx, "tok-alice")
	if err != nil {
		t.Fatalf("by token: %v", err)
	}
	if got.ID != p.ID || got.Name != "alice" {
		t.Fatalf("player mismatch: %+v", got)
	}
	if _, err := st.PlayerByToken(ctx, "wrong"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInviteRedeemSingleUse(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	inv, err := st.CreateInvite(ctx, "ABCD2345", RolePlayer)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	p, err := st.RedeemInvite(ctx, inv.Code, "bob", "tok-bob")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if p.Role != RolePlayer {
		t.Fatalf("role = %s", p.Role)
	}
	// Second redemption must fail.
	if _, err := st.RedeemInvite(ctx, inv.Code, "carol", "tok-carol"); err == nil {
		t.Fatal("expected error redeeming used invite")
	}
	// Duplicate name must fail.
	inv2, _ := st.CreateInvite(ctx, "EFGH6789", RolePlayer)
	if _, err := st.RedeemInvite(ctx, inv2.Code, "bob", "tok-bob2"); err == nil {
		t.Fatal("expected error on duplicate name")
	}
}

func TestRequestLifecycle(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	p, _ := st.CreatePlayer(ctx, "alice", "tok", RolePlayer)

	req, err := st.CreateRequest(ctx, p.ID, "add sodium")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if req.Status != StatusQueued || req.PlayerName != "alice" {
		t.Fatalf("unexpected request: %+v", req)
	}

	// Active request is the one in flight.
	if _, ok, _ := st.ActiveRequest(ctx); !ok {
		t.Fatal("expected an active request")
	}

	// Legal transition.
	if _, err := st.SetStatus(ctx, req.ID, StatusPlanning); err != nil {
		t.Fatalf("queued->planning: %v", err)
	}
	// Illegal transition.
	if _, err := st.SetStatus(ctx, req.ID, StatusPublished); err == nil {
		t.Fatal("expected illegal transition to fail")
	}
	// Terminal.
	if _, err := st.SetStatus(ctx, req.ID, StatusInfeasible, WithError("no such mod")); err != nil {
		t.Fatalf("planning->infeasible: %v", err)
	}
	if _, ok, _ := st.ActiveRequest(ctx); ok {
		t.Fatal("terminal request should not be active")
	}
}

func TestEventsAndVersions(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	seq, err := st.AppendEvent(ctx, EventServerStatus, []byte(`{"state":"running"}`))
	if err != nil || seq != 1 {
		t.Fatalf("append event: seq=%d err=%v", seq, err)
	}
	st.AppendEvent(ctx, EventVersionPublished, []byte(`{"version":1,"summary":"x"}`))
	evs, err := st.EventsAfter(ctx, 0, 10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events after: %d %v", len(evs), err)
	}
	evs, _ = st.EventsAfter(ctx, 1, 10)
	if len(evs) != 1 || evs[0].Type != EventVersionPublished {
		t.Fatalf("cursor filter failed: %+v", evs)
	}

	if err := st.CreateVersion(ctx, Version{Number: 1, GitCommit: "abc", Summary: "init", ManifestPath: "/m/1.json"}); err != nil {
		t.Fatalf("create version: %v", err)
	}
	v, ok, err := st.CurrentVersion(ctx)
	if err != nil || !ok || v.Number != 1 {
		t.Fatalf("current version: %+v ok=%v err=%v", v, ok, err)
	}
}

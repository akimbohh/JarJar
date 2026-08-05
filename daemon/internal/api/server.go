// Package api implements the jarjard HTTP API (docs/API.md): bearer-token auth,
// onboarding, requests, pack/manifest/blob sync, long-poll events, and the
// admin surface. Pack and Jobs are injected as interfaces so the api package
// does not depend on internal/pack or internal/jobs directly.
package api

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/bootstrap"
	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// Version is the daemon semver, set from main via SetVersion.
var Version = "0.0.0-dev"

// MinClient is the minimum acceptable client semver (X-JarJar-Min-Client).
var MinClient = "0.0.0"

// PackMeta is the pack summary returned by /pack/current and embedded in health.
type PackMeta struct {
	Name      string `json:"name"`
	MCVersion string `json:"mc_version"`
	Loader    Loader `json:"loader"`
}

type Loader struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// ChangelogEntry mirrors manifest.version.changelog[].
type ChangelogEntry struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Pack is the read surface the API needs from internal/pack.
type Pack interface {
	// Meta returns pack metadata; ok=false before the first import.
	Meta(ctx context.Context) (meta PackMeta, ok bool, err error)
	// ManifestBytes returns the raw JSON manifest for version n.
	ManifestBytes(ctx context.Context, n int) ([]byte, error)
	// Changelog returns the changelog entries recorded in version n's manifest.
	Changelog(ctx context.Context, n int) ([]ChangelogEntry, error)
	// OpenBlob opens a stored blob by sha256 for range-capable serving.
	OpenBlob(sha string) (rs io.ReadSeekCloser, size int64, err error)
	// BlobRetained reports whether the blob is referenced by a retained manifest.
	BlobRetained(sha string) bool
}

// Jobs is the control surface the API needs from internal/jobs.
type Jobs interface {
	// Wake nudges the runner to pick up newly queued work.
	Wake()
	// EnqueueRollback creates a rollback job to the given version, attributed to
	// playerID (the requesting admin), and returns the synthetic request id.
	EnqueueRollback(ctx context.Context, toVersion int, playerID string) (string, error)
	// Snapshot returns current queue state for admin status.
	Snapshot() JobsSnapshot
}

// JobsSnapshot is returned by Jobs.Snapshot for /admin/status.
type JobsSnapshot struct {
	Queue      []string `json:"queue"`
	CurrentJob *string  `json:"current_job"`
}

// MCStatus is the server-state reader the API needs from internal/mcserver.
type MCStatus interface {
	// State returns "running", "restarting", or "down".
	State(ctx context.Context) string
}

// Setup is the app-driven one-time setup surface (internal/bootstrap.Runner).
type Setup interface {
	// Status returns configured/running state plus live progress.
	Status(ctx context.Context) bootstrap.SetupStatus
	// Start kicks off a setup run in the background.
	Start(ctx context.Context, req bootstrap.SetupRequest) error
}

type Server struct {
	cfg      config.Config
	store    *store.Store
	pack     Pack
	jobs     Jobs
	mc       MCStatus
	setup    Setup
	limiter  *rateLimiter
	mux      *http.ServeMux
	diskFree func(path string) uint64
}

func New(cfg config.Config, st *store.Store, pk Pack, jb Jobs, mc MCStatus, setup Setup) *Server {
	s := &Server{
		cfg:      cfg,
		store:    st,
		pack:     pk,
		jobs:     jb,
		mc:       mc,
		setup:    setup,
		limiter:  newRateLimiter(60, time.Minute),
		diskFree: diskFreeBytes,
	}
	s.routes()
	return s
}

// Handler returns the root http.Handler (with global middleware applied).
func (s *Server) Handler() http.Handler {
	return s.withGlobal(s.mux)
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	// Public (no auth).
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/join", s.handleJoin)

	// Authenticated.
	mux.Handle("POST /api/v1/requests", s.auth(store.RolePlayer, s.handleCreateRequest))
	mux.Handle("GET /api/v1/requests", s.auth(store.RolePlayer, s.handleListRequests))
	mux.Handle("GET /api/v1/requests/{id}", s.auth(store.RolePlayer, s.handleGetRequest))
	mux.Handle("POST /api/v1/requests/{id}/answer", s.auth(store.RolePlayer, s.handleAnswer))

	mux.Handle("GET /api/v1/pack/current", s.auth(store.RolePlayer, s.handlePackCurrent))
	mux.Handle("GET /api/v1/pack/mods", s.auth(store.RolePlayer, s.handlePackMods))
	mux.Handle("GET /api/v1/pack/manifests/{n}", s.auth(store.RolePlayer, s.handleManifest))
	mux.Handle("GET /api/v1/blobs/{sha}", s.authNoLimit(store.RolePlayer, s.handleBlob))
	mux.Handle("HEAD /api/v1/blobs/{sha}", s.authNoLimit(store.RolePlayer, s.handleBlob))
	mux.Handle("GET /api/v1/versions", s.auth(store.RolePlayer, s.handleVersions))

	mux.Handle("GET /api/v1/events", s.auth(store.RolePlayer, s.handleEvents))

	// Admin.
	mux.Handle("POST /api/v1/admin/invites", s.auth(store.RoleAdmin, s.handleCreateInvite))
	mux.Handle("GET /api/v1/admin/players", s.auth(store.RoleAdmin, s.handleListPlayers))
	mux.Handle("DELETE /api/v1/admin/players/{id}", s.auth(store.RoleAdmin, s.handleDeletePlayer))
	mux.Handle("POST /api/v1/admin/requests/{id}/approve", s.auth(store.RoleAdmin, s.handleApprove))
	mux.Handle("POST /api/v1/admin/requests/{id}/reject", s.auth(store.RoleAdmin, s.handleReject))
	mux.Handle("POST /api/v1/admin/rollback", s.auth(store.RoleAdmin, s.handleRollback))
	mux.Handle("GET /api/v1/admin/status", s.auth(store.RoleAdmin, s.handleAdminStatus))
	mux.Handle("GET /api/v1/admin/setup", s.auth(store.RoleAdmin, s.handleGetSetup))
	mux.Handle("POST /api/v1/admin/setup", s.auth(store.RoleAdmin, s.handleStartSetup))

	s.mux = mux
}

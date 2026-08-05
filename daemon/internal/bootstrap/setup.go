package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// SetupRequest is the app-driven one-time setup payload (POST /admin/setup).
type SetupRequest struct {
	Description     string `json:"description"`
	ClaudeToken     string `json:"claude_token"`
	MemoryMB        int    `json:"memory_mb"`
	ServerDir       string `json:"server_dir"`
	ServerPort      int    `json:"server_port"`
	AllowCurseForge bool   `json:"allow_curseforge"`
	CurseForgeKey   string `json:"curseforge_key"`
}

// SetupStep is one progress line of a setup run.
type SetupStep struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
	At      string `json:"at"`
}

// SetupPack is the resulting pack summary once configured.
type SetupPack struct {
	Name      string `json:"name"`
	MCVersion string `json:"mc_version"`
	Loader    struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"loader"`
	Version int `json:"version"`
}

// SetupStatus is what GET /admin/setup returns: enough for the app to decide
// between the onboarding wizard and the dashboard, and to render live progress.
type SetupStatus struct {
	Configured bool        `json:"configured"`
	Running    bool        `json:"running"`
	Phase      string      `json:"phase"`
	Steps      []SetupStep `json:"steps"`
	Pack       *SetupPack  `json:"pack"`
	InviteCode string      `json:"invite_code,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Runner drives the app-initiated one-time setup. It runs bootstrap.Init in the
// background, tracks progress in memory, and mirrors each step onto the store
// event stream so connected clients update live.
type Runner struct {
	cfg     config.Config
	cfgPath string
	st      *store.Store
	pk      *pack.Pack
	log     *slog.Logger

	mu      sync.Mutex
	running bool
	phase   string
	steps   []SetupStep
	err     string
	invite  string
}

// NewRunner constructs a setup Runner over the daemon's shared dependencies.
func NewRunner(cfg config.Config, cfgPath string, st *store.Store, pk *pack.Pack, log *slog.Logger) *Runner {
	return &Runner{cfg: cfg, cfgPath: cfgPath, st: st, pk: pk, log: log}
}

// Status returns a snapshot for the API. It reports Configured from whether a
// pack version exists, so a restart mid-life still shows the dashboard.
func (r *Runner) Status(ctx context.Context) SetupStatus {
	r.mu.Lock()
	s := SetupStatus{
		Running:    r.running,
		Phase:      r.phase,
		Steps:      append([]SetupStep(nil), r.steps...),
		Error:      r.err,
		InviteCode: r.invite,
	}
	r.mu.Unlock()

	if m, ok, err := r.pk.MetaFull(ctx); err == nil && ok {
		s.Configured = true
		sp := &SetupPack{Name: m.Name, MCVersion: m.MCVersion}
		sp.Loader.ID, sp.Loader.Version = m.Loader.ID, m.Loader.Version
		if v, ok, _ := r.st.CurrentVersion(ctx); ok {
			sp.Version = v.Number
		}
		s.Pack = sp
	}
	return s
}

// Start launches a setup run in the background. It rejects the call if setup is
// already running or the pack is already configured.
func (r *Runner) Start(ctx context.Context, req SetupRequest) error {
	if strings.TrimSpace(req.Description) == "" {
		return fmt.Errorf("description is required")
	}
	if strings.TrimSpace(req.ClaudeToken) == "" {
		return fmt.Errorf("claude_token is required")
	}
	if s := r.Status(ctx); s.Configured {
		return fmt.Errorf("already configured")
	}

	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("setup already in progress")
	}
	r.running = true
	r.err = ""
	r.invite = ""
	r.phase = "starting"
	r.steps = nil
	r.mu.Unlock()

	// Persist the Claude token so the headless genesis run can read it.
	if err := r.writeToken(req.ClaudeToken); err != nil {
		r.finish("", fmt.Errorf("store token: %w", err))
		return nil
	}

	// Build the effective config for this run.
	cfg := r.cfg
	if req.ServerDir != "" {
		cfg.Minecraft.ServerDir = req.ServerDir
	}
	if cfg.Minecraft.ServerDir == "" {
		cfg.Minecraft.ServerDir = "/opt/minecraft"
	}
	cfg.Pipeline.AllowCurseForge = req.AllowCurseForge
	cfg.Pipeline.CurseForgeAPIKey = req.CurseForgeKey

	go r.run(cfg, req)
	return nil
}

// run executes bootstrap.Init and records the outcome. It uses a detached
// context so the setup survives the originating HTTP request; genesis + boot
// have their own timeouts.
func (r *Runner) run(cfg config.Config, req SetupRequest) {
	ctx := context.Background()
	res, err := Init(ctx, Options{
		Cfg:            cfg,
		ConfigPath:     r.cfgPath,
		Description:    req.Description,
		MemoryMB:       req.MemoryMB,
		ServerPort:     req.ServerPort,
		InstallSystemd: true,
		Boot:           true,
		Store:          r.st,
		Report:         r.report,
		Log:            r.log,
	})
	if err != nil {
		r.finish("", err)
		return
	}
	r.finish(res.InviteCode, nil)
}

// report records a progress step and mirrors it onto the event stream.
func (r *Runner) report(p Progress) {
	step := SetupStep{Phase: p.Phase, Message: p.Message, Error: p.Error, At: time.Now().UTC().Format(time.RFC3339)}
	r.mu.Lock()
	r.phase = p.Phase
	r.steps = append(r.steps, step)
	r.mu.Unlock()
	r.emit(step)
}

// finish marks the run complete (success carries the admin invite; failure the
// error) and emits a terminal event.
func (r *Runner) finish(invite string, err error) {
	final := SetupStep{Phase: "done", At: time.Now().UTC().Format(time.RFC3339)}
	r.mu.Lock()
	r.running = false
	if err != nil {
		r.err = err.Error()
		r.phase = "error"
		final.Phase = "error"
		final.Error = err.Error()
		final.Message = "Setup failed."
	} else {
		r.invite = invite
		r.phase = "done"
		final.Message = "Setup complete."
	}
	r.steps = append(r.steps, final)
	r.mu.Unlock()
	if err != nil {
		r.log.Error("setup failed", "err", err)
	} else {
		r.log.Info("setup complete")
	}
	r.emit(final)
}

func (r *Runner) emit(step SetupStep) {
	buf, _ := json.Marshal(step)
	if _, err := r.st.AppendEvent(context.Background(), store.EventSetupProgress, buf); err != nil {
		r.log.Warn("emit setup event", "err", err)
	}
}

func (r *Runner) writeToken(token string) error {
	path := r.cfg.Claude.TokenFile
	if path == "" {
		path = filepath.Join(r.cfg.Server.DataDir, "secrets", "claude-token")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

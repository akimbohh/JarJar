// Package mcserver controls the Minecraft server process (systemd unit or
// configured commands) and talks RCON for player count and warnings. The
// daemon never embeds the game server; it manages an external unit
// (ARCHITECTURE.md §2).
package mcserver

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/config"
)

// State values reported to clients.
const (
	StateRunning    = "running"
	StateRestarting = "restarting"
	StateDown       = "down"
)

type Controller struct {
	cfg config.MinecraftConfig

	// restarting is set while a managed restart is in progress so State() can
	// report "restarting" rather than briefly "down".
	restarting atomic.Bool

	mu sync.Mutex
}

func New(cfg config.MinecraftConfig) *Controller {
	return &Controller{cfg: cfg}
}

// State returns "running", "restarting", or "down".
func (c *Controller) State(ctx context.Context) string {
	if c.restarting.Load() {
		return StateRestarting
	}
	if c.isActive(ctx) {
		return StateRunning
	}
	return StateDown
}

// isActive reports whether the server process is up. For systemd it checks the
// unit; otherwise it probes RCON.
func (c *Controller) isActive(ctx context.Context) bool {
	if c.cfg.Control == "systemd" {
		out, _ := c.systemctl(ctx, "is-active", c.cfg.SystemdUnit)
		return strings.TrimSpace(out) == "active"
	}
	// command mode: treat a successful RCON connect as "up".
	if _, err := rconCommand(ctx, c.cfg.RconAddr, c.cfg.RconPassword, "list"); err == nil {
		return true
	}
	return false
}

// PlayerCount returns the number of players online via RCON.
func (c *Controller) PlayerCount(ctx context.Context) (int, error) {
	resp, err := rconCommand(ctx, c.cfg.RconAddr, c.cfg.RconPassword, "list")
	if err != nil {
		return 0, err
	}
	n, ok := parsePlayerCount(resp)
	if !ok {
		return 0, fmt.Errorf("could not parse player list: %q", resp)
	}
	return n, nil
}

// Say broadcasts a message in-game (best effort).
func (c *Controller) Say(ctx context.Context, msg string) {
	rconCommand(ctx, c.cfg.RconAddr, c.cfg.RconPassword, "say "+msg)
}

// Stop stops the server, allowing time for the world to save.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.Control == "systemd" {
		_, err := c.systemctl(ctx, "stop", c.cfg.SystemdUnit)
		return err
	}
	return c.runShell(ctx, c.cfg.StopCommand)
}

// Start starts the server.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.Control == "systemd" {
		_, err := c.systemctl(ctx, "start", c.cfg.SystemdUnit)
		return err
	}
	return c.runShell(ctx, c.cfg.StartCommand)
}

// WaitEmpty blocks until the server reports 0 players, polling every 60s. It
// returns nil when empty, or after the configured when_empty timeout (forcing
// the restart). ctx cancellation returns ctx.Err().
func (c *Controller) WaitEmpty(ctx context.Context, warn func(remaining time.Duration)) error {
	deadline := time.Now().Add(time.Duration(c.cfg.WhenEmptyTimeoutMinutes) * time.Minute)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		if n, err := c.PlayerCount(ctx); err == nil && n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return nil // forced restart per policy
		}
		if warn != nil {
			warn(time.Until(deadline))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// WaitHealthy waits until the server is active and RCON connects, up to the
// configured boot-healthy timeout.
func (c *Controller) WaitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(time.Duration(c.cfg.BootHealthyTimeoutSecs) * time.Second)
	for {
		if c.isActive(ctx) {
			if _, err := rconCommand(ctx, c.cfg.RconAddr, c.cfg.RconPassword, "list"); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server did not become healthy within %ds", c.cfg.BootHealthyTimeoutSecs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// SetRestarting toggles the restarting flag (worker sets it around a restart).
func (c *Controller) SetRestarting(v bool) { c.restarting.Store(v) }

func (c *Controller) systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (c *Controller) runShell(ctx context.Context, command string) error {
	if command == "" {
		return fmt.Errorf("no command configured")
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command %q: %w: %s", command, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

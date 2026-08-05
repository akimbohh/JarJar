package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "embed"

	"github.com/akimbohh/jarjar/daemon/internal/config"
)

//go:embed assets/genesis.md
var genesisPrompt string

//go:embed assets/genesis.schema.json
var genesisSchema string

// GenesisPlan is the AI's decision when creating a pack from a plain-English
// description. It mirrors schemas/genesis.schema.json.
type GenesisPlan struct {
	SchemaVersion int           `json:"schema_version"`
	MCVersion     string        `json:"mc_version"`
	Loader        GenesisLoader `json:"loader"`
	Source        GenesisSource `json:"source"`
	PackName      string        `json:"pack_name"`
	Summary       string        `json:"summary"`
}

type GenesisLoader struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// GenesisSource is a discriminated union: either an existing published pack to
// import, or a from-scratch mod list to assemble.
type GenesisSource struct {
	Type string `json:"type"` // "existing_pack" | "scratch"

	// existing_pack
	Platform  string `json:"platform,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`

	// scratch
	Mods []GenesisMod `json:"mods,omitempty"`
}

type GenesisMod struct {
	Platform    string `json:"platform"`
	ProjectID   string `json:"project_id"`
	ProjectSlug string `json:"project_slug"`
	VersionID   string `json:"version_id"`
}

// claudeResult is the JSON emitted by `claude -p --output-format json` (the
// subset genesis needs). It matches worker.claudeResult.
type claudeResult struct {
	StructuredOutput json.RawMessage `json:"structured_output"`
	NumTurns         int             `json:"num_turns"`
	StopReason       string          `json:"stop_reason"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	IsError          bool            `json:"is_error"`
}

// Genesis runs the headless genesis-planning stage: Claude Code decides the mc
// version, loader, and whether to import an existing pack or build from scratch,
// citing registry ids it verified with `jarjard modtool`. It runs in a throwaway
// working directory with the registry CLI as its only tool.
func Genesis(ctx context.Context, cfg config.Config, description string) (GenesisPlan, error) {
	if strings.TrimSpace(description) == "" {
		return GenesisPlan{}, fmt.Errorf("genesis: empty pack description")
	}

	work, err := os.MkdirTemp("", "jarjar-genesis-*")
	if err != nil {
		return GenesisPlan{}, err
	}
	defer os.RemoveAll(work)

	prompt := strings.ReplaceAll(genesisPrompt, "{{description}}", description)

	timeout := time.Duration(cfg.Claude.TimeoutMinutes) * time.Minute
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--json-schema", genesisSchema,
		"--model", cfg.Claude.Model,
		"--max-turns", itoa(cfg.Claude.MaxTurns),
		// The registry CLI is the only tool; no file edits, no web access.
		"--allowedTools", "Bash(jarjard modtool *)",
		"--disallowedTools", "Read,Edit,Write,WebFetch,WebSearch,Task,NotebookEdit",
		"--permission-mode", "acceptEdits",
		"--mcp-config", `{"mcpServers":{}}`,
		"--settings", `{"permissions":{"deny":["Read(//var/lib/jarjar/secrets/**)"]}}`,
	}

	cmd := exec.CommandContext(cctx, cfg.Claude.Binary, args...)
	cmd.Dir = work
	cmd.Env = claudeEnv(cfg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return GenesisPlan{}, fmt.Errorf("genesis: claude timed out after %s", timeout)
		}
		return GenesisPlan{}, fmt.Errorf("genesis: claude exited with error: %v: %s", err, tail(stderr.String(), 400))
	}

	var res claudeResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return GenesisPlan{}, fmt.Errorf("genesis: parse claude JSON output: %w: %s", err, tail(stdout.String(), 400))
	}
	if res.StopReason == "max_turns" || res.StopReason == "error_max_turns" {
		return GenesisPlan{}, fmt.Errorf("genesis: planning exhausted the turn limit without producing a plan")
	}
	if len(res.StructuredOutput) == 0 {
		return GenesisPlan{}, fmt.Errorf("genesis: claude produced no structured plan output")
	}
	var plan GenesisPlan
	if err := json.Unmarshal(res.StructuredOutput, &plan); err != nil {
		return GenesisPlan{}, fmt.Errorf("genesis: parse structured plan: %w", err)
	}
	if err := plan.validate(); err != nil {
		return GenesisPlan{}, err
	}
	return plan, nil
}

// validate enforces the cross-field invariants the JSON Schema oneOf encodes,
// so a malformed plan fails here rather than deep in the build.
func (p GenesisPlan) validate() error {
	if p.MCVersion == "" {
		return fmt.Errorf("genesis: plan has empty mc_version")
	}
	switch p.Loader.ID {
	case "fabric", "neoforge", "forge", "quilt":
	default:
		return fmt.Errorf("genesis: unsupported loader %q", p.Loader.ID)
	}
	switch p.Source.Type {
	case "existing_pack":
		if p.Source.Platform == "" || p.Source.ProjectID == "" || p.Source.VersionID == "" {
			return fmt.Errorf("genesis: existing_pack source is missing platform/project_id/version_id")
		}
	case "scratch":
		if len(p.Source.Mods) == 0 {
			return fmt.Errorf("genesis: scratch source lists no mods")
		}
		for i, m := range p.Source.Mods {
			if m.Platform == "" || m.ProjectID == "" || m.VersionID == "" {
				return fmt.Errorf("genesis: scratch mod #%d is missing platform/project_id/version_id", i+1)
			}
		}
	default:
		return fmt.Errorf("genesis: unknown source type %q", p.Source.Type)
	}
	if p.PackName == "" {
		return fmt.Errorf("genesis: plan has empty pack_name")
	}
	return nil
}

// claudeEnv builds the sanitized environment for the headless genesis run,
// mirroring worker.Deps.claudeEnv: minimal base, subscription token, isolated
// HOME/config, and the modtool CF settings.
func claudeEnv(cfg config.Config) []string {
	token, _ := os.ReadFile(cfg.Claude.TokenFile)
	dataDir := cfg.Server.DataDir
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"TERM=dumb",
		"HOME=" + filepath.Join(dataDir, "claude-home"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dataDir, "claude-config"),
		"CLAUDE_CODE_OAUTH_TOKEN=" + strings.TrimSpace(string(token)),
		"JARJAR_ALLOW_CF=" + boolEnv(cfg.Pipeline.AllowCurseForge),
		"JARJAR_CF_KEY=" + cfg.Pipeline.CurseForgeAPIKey,
	}
	os.MkdirAll(filepath.Join(dataDir, "claude-home"), 0o700)
	os.MkdirAll(filepath.Join(dataDir, "claude-config"), 0o700)
	return env
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

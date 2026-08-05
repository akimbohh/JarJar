package worker

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

	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

//go:embed assets/CLAUDE.pack.md
var claudeMDTemplate string

//go:embed assets/planner.md
var plannerTemplate string

//go:embed assets/plan.schema.json
var planSchema string

// writeClaudeMD renders assets/CLAUDE.pack.md into the worktree as CLAUDE.md and
// stages a read-only snapshot of server.properties under .jarjar/.
func (d Deps) writeClaudeMD(ctx context.Context, worktree string) error {
	meta, err := pack.ReadMetaFrom(worktree)
	if err != nil {
		return fmt.Errorf("read pack meta: %w", err)
	}
	body := renderTemplate(claudeMDTemplate, map[string]string{
		"pack_name":      meta.Name,
		"mc_version":     meta.MCVersion,
		"loader_id":      meta.Loader.ID,
		"loader_version": meta.Loader.Version,
	})
	if err := os.WriteFile(filepath.Join(worktree, "CLAUDE.md"), []byte(body), 0o644); err != nil {
		return err
	}
	jj := filepath.Join(worktree, ".jarjar")
	if err := os.MkdirAll(jj, 0o755); err != nil {
		return err
	}
	// Snapshot server.properties (read-only informational copy).
	if buf, err := os.ReadFile(filepath.Join(d.Cfg.Minecraft.ServerDir, "server.properties")); err == nil {
		os.WriteFile(filepath.Join(jj, "server.properties"), buf, 0o444)
	}
	return nil
}

// claudeResult is the JSON emitted by `claude -p --output-format json`.
type claudeResult struct {
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	NumTurns         int             `json:"num_turns"`
	StopReason       string          `json:"stop_reason"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            map[string]any  `json:"usage"`
	IsError          bool            `json:"is_error"`
}

// plan runs the headless planning stage and returns the parsed plan.
func (d Deps) plan(ctx context.Context, worktree string, req store.Request) (Plan, error) {
	meta, err := pack.ReadMetaFrom(worktree)
	if err != nil {
		return Plan{}, err
	}

	qa := ""
	if req.ClarificationQuestion != nil && req.ClarificationAnswer != nil {
		qa = fmt.Sprintf("\nEarlier you asked: %q\nThe player answered: %q\n", *req.ClarificationQuestion, *req.ClarificationAnswer)
	}
	prompt := renderTemplate(plannerTemplate, map[string]string{
		"pack_name":        meta.Name,
		"mc_version":       meta.MCVersion,
		"loader_id":        meta.Loader.ID,
		"loader_version":   meta.Loader.Version,
		"player_name":      req.PlayerName,
		"request_text":     req.Text,
		"clarification_qa": qa,
	})

	timeout := time.Duration(d.Cfg.Claude.TimeoutMinutes) * time.Minute
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--json-schema", planSchema,
		"--model", d.Cfg.Claude.Model,
		"--max-turns", itoa(d.Cfg.Claude.MaxTurns),
		"--allowedTools", "Read,Edit,Write,Glob,Grep,Bash(jarjard modtool *)",
		"--disallowedTools", "WebFetch,WebSearch,Task,NotebookEdit",
		"--permission-mode", "acceptEdits",
		"--mcp-config", `{"mcpServers":{}}`,
		"--settings", `{"permissions":{"deny":["Read(//var/lib/jarjar/secrets/**)","Edit(//var/lib/jarjar/secrets/**)","Read(~/.ssh/**)"]}}`,
	}

	cmd := exec.CommandContext(cctx, d.Cfg.Claude.Binary, args...)
	cmd.Dir = worktree
	cmd.Env = d.claudeEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return Plan{}, fmt.Errorf("claude timed out after %s", timeout)
		}
		return Plan{}, fmt.Errorf("claude exited with error: %v: %s", err, tail(stderr.String(), 400))
	}

	var res claudeResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return Plan{}, fmt.Errorf("parse claude JSON output: %w: %s", err, tail(stdout.String(), 400))
	}
	d.Log.Info("planning run complete", "req", req.ID, "turns", res.NumTurns,
		"cost_usd", res.TotalCostUSD, "stop_reason", res.StopReason)
	if res.StopReason == "max_turns" || res.StopReason == "error_max_turns" {
		return Plan{}, fmt.Errorf("planning exhausted the turn limit without producing a plan")
	}
	if len(res.StructuredOutput) == 0 {
		return Plan{}, fmt.Errorf("claude produced no structured plan output")
	}
	var plan Plan
	if err := json.Unmarshal(res.StructuredOutput, &plan); err != nil {
		return Plan{}, fmt.Errorf("parse structured plan: %w", err)
	}
	return plan, nil
}

// claudeEnv builds a sanitized environment for the headless run (PIPELINE.md §4):
// minimal base, API keys explicitly unset, subscription token + config isolation.
func (d Deps) claudeEnv() []string {
	token, _ := os.ReadFile(d.Cfg.Claude.TokenFile)
	dataDir := d.Cfg.Server.DataDir
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"TERM=dumb",
		"HOME=" + filepath.Join(dataDir, "claude-home"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dataDir, "claude-config"),
		"CLAUDE_CODE_OAUTH_TOKEN=" + strings.TrimSpace(string(token)),
		// modtool CF settings for the model's registry window.
		"JARJAR_ALLOW_CF=" + boolEnv(d.Cfg.Pipeline.AllowCurseForge),
		"JARJAR_CF_KEY=" + d.Cfg.Pipeline.CurseForgeAPIKey,
	}
	// Ensure the isolation dirs exist.
	os.MkdirAll(filepath.Join(dataDir, "claude-home"), 0o700)
	os.MkdirAll(filepath.Join(dataDir, "claude-config"), 0o700)
	return env
}

func renderTemplate(tpl string, vars map[string]string) string {
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
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

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

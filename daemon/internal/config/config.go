// Package config loads and validates the daemon configuration
// (/etc/jarjar/jarjard.toml). The shape mirrors schemas/jarjard-config.schema.json.
package config

import (
	"fmt"
	"net"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server    ServerConfig    `toml:"server"`
	Minecraft MinecraftConfig `toml:"minecraft"`
	Claude    ClaudeConfig    `toml:"claude"`
	Pipeline  PipelineConfig  `toml:"pipeline"`
}

type ServerConfig struct {
	ListenAddr  string `toml:"listen_addr"`
	PublicURL   string `toml:"public_url"`
	TLSCertFile string `toml:"tls_cert_file"`
	TLSKeyFile  string `toml:"tls_key_file"`
	DataDir     string `toml:"data_dir"`
}

type MinecraftConfig struct {
	ServerDir               string `toml:"server_dir"`
	Control                 string `toml:"control"`
	SystemdUnit             string `toml:"systemd_unit"`
	StartCommand            string `toml:"start_command"`
	StopCommand             string `toml:"stop_command"`
	RconAddr                string `toml:"rcon_addr"`
	RconPassword            string `toml:"rcon_password"`
	RestartPolicy           string `toml:"restart_policy"`
	WhenEmptyTimeoutMinutes int    `toml:"when_empty_timeout_minutes"`
	BootHealthyTimeoutSecs  int    `toml:"boot_healthy_timeout_seconds"`
}

type ClaudeConfig struct {
	Binary         string `toml:"binary"`
	Model          string `toml:"model"`
	TokenFile      string `toml:"token_file"`
	MaxTurns       int    `toml:"max_turns"`
	TimeoutMinutes int    `toml:"timeout_minutes"`
	MemoryMax      string `toml:"memory_max"`
}

type PipelineConfig struct {
	RequireApproval  bool   `toml:"require_approval"`
	AllowCurseForge  bool   `toml:"allow_curseforge"`
	CurseForgeAPIKey string `toml:"curseforge_api_key"`
}

// Default returns a Config populated with schema defaults. Load overlays a file
// on top of these.
func Default() Config {
	return Config{
		Server: ServerConfig{
			ListenAddr: "0.0.0.0:25580",
			DataDir:    "/var/lib/jarjar",
		},
		Minecraft: MinecraftConfig{
			Control:                 "systemd",
			SystemdUnit:             "minecraft.service",
			RconAddr:                "127.0.0.1:25575",
			RestartPolicy:           "when_empty",
			WhenEmptyTimeoutMinutes: 720,
			BootHealthyTimeoutSecs:  300,
		},
		Claude: ClaudeConfig{
			Binary:         "claude",
			Model:          "sonnet",
			TokenFile:      "/var/lib/jarjar/secrets/claude-token",
			MaxTurns:       50,
			TimeoutMinutes: 20,
			MemoryMax:      "1536M",
		},
	}
}

// Load reads path, overlaying it on Default, then validates.
func Load(path string) (Config, error) {
	cfg := Default()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("config file %s does not exist", path)
		}
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	// Claude token file defaults relative to data_dir if data_dir was overridden
	// but token_file left at the compile-time default.
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate enforces the invariants from the JSON Schema that TOML decoding does
// not (enums, required cross-field constraints).
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.ListenAddr); err != nil {
		return fmt.Errorf("server.listen_addr %q is not host:port: %w", c.Server.ListenAddr, err)
	}
	if (c.Server.TLSCertFile == "") != (c.Server.TLSKeyFile == "") {
		return fmt.Errorf("server.tls_cert_file and server.tls_key_file must be set together")
	}
	if c.Server.DataDir == "" {
		return fmt.Errorf("server.data_dir is required")
	}
	if c.Minecraft.ServerDir == "" {
		return fmt.Errorf("minecraft.server_dir is required")
	}
	switch c.Minecraft.Control {
	case "systemd":
	case "command":
		if c.Minecraft.StartCommand == "" || c.Minecraft.StopCommand == "" {
			return fmt.Errorf("minecraft.start_command and stop_command are required when control = command")
		}
	default:
		return fmt.Errorf("minecraft.control must be systemd or command, got %q", c.Minecraft.Control)
	}
	switch c.Minecraft.RestartPolicy {
	case "immediate", "when_empty":
	default:
		return fmt.Errorf("minecraft.restart_policy must be immediate or when_empty, got %q", c.Minecraft.RestartPolicy)
	}
	if c.Claude.MaxTurns < 5 {
		return fmt.Errorf("claude.max_turns must be >= 5")
	}
	if c.Claude.TimeoutMinutes < 2 {
		return fmt.Errorf("claude.timeout_minutes must be >= 2")
	}
	if c.Pipeline.AllowCurseForge && c.Pipeline.CurseForgeAPIKey == "" {
		return fmt.Errorf("pipeline.curseforge_api_key is required when allow_curseforge is true")
	}
	return nil
}

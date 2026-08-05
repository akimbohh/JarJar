package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akimbohh/jarjar/daemon/internal/bootstrap"
	"github.com/akimbohh/jarjar/daemon/internal/config"
)

// cmdInit implements `jarjard init` — one-time setup for an optimized, always-on
// server. Given a plain-English description it plans the pack (genesis),
// provisions the server (Aikar's flags, tuned server.properties, always-restart
// unit), builds version 1, seeds the server dir, installs+boots the service, and
// prints the first invite code.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to write jarjard.toml")
	desc := fs.String("description", "", "plain-English description of the pack to create")
	dataDir := fs.String("data-dir", "/var/lib/jarjar", "daemon data directory")
	serverDir := fs.String("server-dir", "/opt/minecraft", "Minecraft server directory")
	listen := fs.String("listen", "0.0.0.0:25580", "daemon listen address")
	publicURL := fs.String("public-url", "", "public URL players use to reach the daemon")
	tokenFile := fs.String("token-file", "", "path to the Claude Code OAuth token (default <data-dir>/secrets/claude-token)")
	token := fs.String("token", "", "Claude Code OAuth token value (written to --token-file)")
	model := fs.String("model", "sonnet", "Claude model for genesis planning")
	memoryMB := fs.Int("memory-mb", 9216, "JVM heap for the server, in MB")
	serverPort := fs.Int("server-port", 25565, "Minecraft server port")
	javaPath := fs.String("java", "java", "path to the java binary")
	serviceUser := fs.String("service-user", "", "systemd User= for the service (empty to omit)")
	allowCF := fs.Bool("allow-curseforge", false, "allow CurseForge as a mod source")
	cfKey := fs.String("curseforge-key", "", "CurseForge API key (required with --allow-curseforge)")
	installSystemd := fs.Bool("install-systemd", true, "write the systemd unit under /etc and enable it (needs root)")
	boot := fs.Bool("boot", true, "start the server and wait for a healthy boot after install")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Allow the description as a trailing positional too: `jarjard init "..."`.
	description := *desc
	if description == "" && fs.NArg() > 0 {
		description = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(description) == "" {
		fmt.Fprintln(os.Stderr, "usage: jarjard init --description \"the pack you want\" [flags]")
		return 2
	}
	if *allowCF && *cfKey == "" {
		fmt.Fprintln(os.Stderr, "--curseforge-key is required with --allow-curseforge")
		return 2
	}

	tf := *tokenFile
	if tf == "" {
		tf = filepath.Join(*dataDir, "secrets", "claude-token")
	}
	if *token != "" {
		if err := os.MkdirAll(filepath.Dir(tf), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if err := os.WriteFile(tf, []byte(strings.TrimSpace(*token)+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "error writing token:", err)
			return 1
		}
	}
	if _, err := os.Stat(tf); err != nil {
		fmt.Fprintf(os.Stderr, "Claude token not found at %s — pass --token or place it there first.\n", tf)
		return 1
	}

	cfg := config.Default()
	cfg.Server.ListenAddr = *listen
	cfg.Server.PublicURL = *publicURL
	cfg.Server.DataDir = *dataDir
	cfg.Minecraft.ServerDir = *serverDir
	cfg.Claude.Model = *model
	cfg.Claude.TokenFile = tf
	cfg.Pipeline.AllowCurseForge = *allowCF
	cfg.Pipeline.CurseForgeAPIKey = *cfKey

	res, err := bootstrap.Init(context.Background(), bootstrap.Options{
		Cfg:            cfg,
		ConfigPath:     *cfgPath,
		Description:    description,
		MemoryMB:       *memoryMB,
		ServerPort:     *serverPort,
		JavaPath:       *javaPath,
		ServiceUser:    *serviceUser,
		InstallSystemd: *installSystemd,
		Boot:           *boot,
		Log:            newLogger(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init failed:", err)
		return 1
	}

	printInitResult(res, *installSystemd)
	return 0
}

func printInitResult(res bootstrap.Result, installedSystemd bool) {
	fmt.Println()
	fmt.Printf("✓ JarJar is ready: %q (Minecraft %s, %s)\n", res.Plan.PackName, res.Plan.MCVersion, res.Plan.Loader.ID)
	fmt.Printf("  %s\n", res.Plan.Summary)
	fmt.Printf("  pack version: %d\n", res.Version)
	if res.Booted {
		fmt.Println("  server: running (healthy)")
	} else if installedSystemd {
		fmt.Printf("  server: installed as %s — start it with `systemctl start %s`\n", res.SystemdUnitName, res.SystemdUnitName)
	} else {
		fmt.Println("  server: provisioned but not installed. Install the unit below, then enable + start it:")
		fmt.Printf("    sudo tee /etc/systemd/system/%s >/dev/null <<'UNIT'\n%s\nUNIT\n", res.SystemdUnitName, res.SystemdUnitContents)
		fmt.Printf("    sudo systemctl daemon-reload && sudo systemctl enable --now %s\n", res.SystemdUnitName)
	}
	fmt.Println()
	fmt.Printf("Admin invite code: %s\n", res.InviteCode)
	if res.Config.Server.PublicURL != "" {
		fmt.Printf("Open the JarJar app, add server %s, and paste this code to join.\n", res.Config.Server.PublicURL)
	} else {
		fmt.Println("Open the JarJar app, add this server's URL, and paste this code to join.")
	}
	fmt.Println("Then run the daemon: `jarjard serve`")
}

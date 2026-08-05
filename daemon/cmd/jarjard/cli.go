package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/id"
	"github.com/akimbohh/jarjar/daemon/internal/importer"
	"github.com/akimbohh/jarjar/daemon/internal/mcserver"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// openStore is a helper for the CLI verbs that operate directly on the DB.
func openStore(cfgPath string) (config.Config, *store.Store, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "db.sqlite"))
	if err != nil {
		return cfg, nil, err
	}
	return cfg, st, nil
}

func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	name := fs.String("name", "", "override pack name")
	cfgPath := fs.String("config", defaultConfigPath, "path to jarjard.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: jarjard import PACKFILE [--name NAME]")
		return 2
	}
	packFile := fs.Arg(0)

	cfg, st, err := openStore(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()

	pk := pack.New(cfg.Server.DataDir)
	ctx := context.Background()
	if err := importer.Import(ctx, cfg, pk, st, packFile, *name); err != nil {
		fmt.Fprintln(os.Stderr, "import failed:", err)
		return 1
	}
	meta, _, _ := pk.MetaFull(ctx)
	fmt.Printf("Imported %q as version 1 (%d mods).\n", meta.Name, len(meta.SideOverrides))
	fmt.Println("Version 1 published. Point players at this server with `jarjard invite`.")
	return 0
}

func cmdInvite(args []string) int {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	role := fs.String("role", "player", "player or admin")
	cfgPath := fs.String("config", defaultConfigPath, "path to jarjard.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *role != store.RolePlayer && *role != store.RoleAdmin {
		fmt.Fprintln(os.Stderr, "role must be player or admin")
		return 2
	}
	cfg, st, err := openStore(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()

	inv, err := st.CreateInvite(context.Background(), id.NewInviteCode(), *role)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create invite failed:", err)
		return 1
	}
	fmt.Printf("Invite code (%s): %s\n", inv.Role, inv.Code)
	if cfg.Server.PublicURL != "" {
		fmt.Printf("Players join with server URL %s and this code.\n", cfg.Server.PublicURL)
	}
	return 0
}

func cmdRollback(args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	to := fs.Int("to", 0, "version number to roll back to")
	cfgPath := fs.String("config", defaultConfigPath, "path to jarjard.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to < 1 {
		fmt.Fprintln(os.Stderr, "usage: jarjard rollback --to N")
		return 2
	}
	_, st, err := openStore(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.VersionByNumber(ctx, *to); err != nil {
		fmt.Fprintln(os.Stderr, "no such version:", *to)
		return 1
	}
	if _, active, _ := st.ActiveRequest(ctx); active {
		fmt.Fprintln(os.Stderr, "another request is in progress; try again later")
		return 1
	}
	// Attribute the rollback to the bootstrap admin.
	admins, _ := st.ListPlayers(ctx)
	var adminID string
	for _, p := range admins {
		if p.Role == store.RoleAdmin {
			adminID = p.ID
			break
		}
	}
	if adminID == "" {
		fmt.Fprintln(os.Stderr, "no admin player found; start the daemon once to bootstrap one")
		return 1
	}
	req, err := st.CreateRequest(ctx, adminID, fmt.Sprintf("Roll back to version %d", *to))
	if err != nil {
		fmt.Fprintln(os.Stderr, "enqueue failed:", err)
		return 1
	}
	planJSON, _ := json.Marshal(map[string]int{"rollback_to": *to})
	st.SetStatus(ctx, req.ID, store.StatusQueued, store.WithPlan(string(planJSON)))
	fmt.Printf("Rollback to version %d queued (request %s). The running daemon will process it.\n", *to, req.ID)
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to jarjard.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, st, err := openStore(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()

	fmt.Println("JarJar daemon status")
	if v, ok, _ := st.CurrentVersion(ctx); ok {
		fmt.Printf("  pack version: %d — %s\n", v.Number, v.Summary)
	} else {
		fmt.Println("  pack version: none (not imported)")
	}

	mc := mcserver.New(cfg.Minecraft)
	fmt.Printf("  minecraft server: %s\n", mc.State(ctx))

	reqs, _ := st.ListRequests(ctx, 100, "")
	active := 0
	for _, r := range reqs {
		if !store.IsTerminal(r.Status) {
			if active == 0 {
				fmt.Println("  queue:")
			}
			active++
			fmt.Printf("    - %s [%s] %s\n", r.ID, r.Status, r.Text)
		}
	}
	if active == 0 {
		fmt.Println("  queue: empty")
	}
	return 0
}

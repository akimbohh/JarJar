package main

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/mcserver"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
	"github.com/akimbohh/jarjar/daemon/internal/worker"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// cmdWorker runs a single pipeline job. Spawned by the serve-side runner as a
// separate process for memory isolation.
func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	jobID := fs.String("job", "", "request id to process")
	cfgPath := fs.String("config", defaultConfigPath, "path to jarjard.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := newLogger()
	if *jobID == "" {
		log.Error("worker: --job is required")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "db.sqlite"))
	if err != nil {
		log.Error("open store", "err", err)
		return 1
	}
	defer st.Close()

	reg := modtool.New(cfg.Pipeline.AllowCurseForge, cfg.Pipeline.CurseForgeAPIKey,
		"jarjar/"+version+" (github.com/akimbohh/JarJar)")

	deps := worker.Deps{
		Cfg:      cfg,
		Store:    st,
		Pack:     pack.New(cfg.Server.DataDir),
		Registry: reg,
		MC:       mcserver.New(cfg.Minecraft),
		Log:      log,
	}
	if err := worker.Run(context.Background(), deps, *jobID); err != nil {
		log.Error("worker run", "job", *jobID, "err", err)
		return 1
	}
	return 0
}

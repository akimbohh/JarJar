package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/api"
	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/id"
	"github.com/akimbohh/jarjar/daemon/internal/jobs"
	"github.com/akimbohh/jarjar/daemon/internal/mcserver"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to jarjard.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := newLogger()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	if err := os.MkdirAll(cfg.Server.DataDir, 0o755); err != nil {
		log.Error("create data dir", "err", err)
		return 1
	}

	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "db.sqlite"))
	if err != nil {
		log.Error("open store", "err", err)
		return 1
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pk := pack.New(cfg.Server.DataDir)
	pk.PruneWorktrees(ctx)
	if err := pk.RefreshRetained(ctx, st, 10); err != nil {
		log.Warn("refresh retained blobs", "err", err)
	}

	if err := bootstrapAdmin(ctx, cfg, st, log); err != nil {
		log.Error("bootstrap admin", "err", err)
		return 1
	}

	mc := mcserver.New(cfg.Minecraft)

	selfExe, _ := os.Executable()
	runner := jobs.New(cfg, st, log, selfExe, *cfgPath)

	srv := api.New(cfg, st, packAPI{pk: pk}, runner, mc)

	// Background goroutines.
	go runner.Run(ctx)
	go eventPoller(ctx, st)
	go maintenance(ctx, pk, st, log)

	httpSrv := &http.Server{
		Addr:              cfg.Server.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		var serveErr error
		if cfg.Server.TLSCertFile != "" {
			log.Info("listening (TLS)", "addr", cfg.Server.ListenAddr)
			serveErr = httpSrv.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			log.Info("listening", "addr", cfg.Server.ListenAddr)
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("http server", "err", serveErr)
			cancel()
		}
	}()

	// Wait for a signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		log.Info("shutting down")
	case <-ctx.Done():
	}
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	httpSrv.Shutdown(shutCtx)
	return 0
}

// bootstrapAdmin ensures an admin exists and a loopback admin token is on disk
// (used by the CLI verbs). Runs once on first start.
func bootstrapAdmin(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) error {
	tokenPath := filepath.Join(cfg.Server.DataDir, "secrets", "admin-token")
	if _, err := os.Stat(tokenPath); err == nil {
		return nil // already bootstrapped
	}
	admins, err := st.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if admins > 0 {
		// Admins exist but no token file (e.g. restored DB); mint a fresh admin.
		log.Warn("admins exist but no admin-token file; minting a new admin for CLI use")
	}
	token := id.NewToken()
	if _, err := st.CreatePlayer(ctx, "admin", token, store.RoleAdmin); err != nil {
		// If name taken, append a suffix.
		if _, err2 := st.CreatePlayer(ctx, "admin-"+id.NewInviteCode(), token, store.RoleAdmin); err2 != nil {
			return err2
		}
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return err
	}
	log.Info("bootstrapped admin token", "path", tokenPath)
	return nil
}

// eventPoller wakes long-poll waiters when the worker process (a separate
// process, invisible to the in-process hub) writes events.
func eventPoller(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if seq, err := st.LatestSeq(ctx); err == nil && seq > last {
				last = seq
				st.Poke(seq)
			}
		}
	}
}

// maintenance runs the daily blob GC and event prune.
func maintenance(ctx context.Context, pk *pack.Pack, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	run := func() {
		if err := pk.RefreshRetained(ctx, st, 10); err == nil {
			if n, err := pk.RunGC(); err == nil && n > 0 {
				log.Info("blob GC", "removed", n)
			}
		}
		if n, err := st.PruneEvents(ctx, time.Now().Add(-30*24*time.Hour)); err == nil && n > 0 {
			log.Info("event prune", "removed", n)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

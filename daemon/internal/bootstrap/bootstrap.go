// Package bootstrap implements `jarjard init`: stand up an optimized, always-on
// modded server from a single plain-English description. It (1) asks Claude Code
// to plan the pack (genesis — find an existing pack or assemble one), (2)
// provisions the server tuned to run continuously — loader install, EULA,
// performance-tuned server.properties with RCON, Aikar's JVM flags, and an
// always-restart systemd unit — (3) builds pack version 1, (4) seeds the server
// directory with the server-side files, (5) installs + boots the service, and
// (6) mints the first invite code. The AI finds the modpack, creates the
// profile, and from there the normal request pipeline edits it. Init is the
// one-time setup; the server itself is meant to stay up.
package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/id"
	"github.com/akimbohh/jarjar/daemon/internal/importer"
	"github.com/akimbohh/jarjar/daemon/internal/mcserver"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/provision"
	"github.com/akimbohh/jarjar/daemon/internal/store"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

const userAgent = "jarjar/dev (github.com/akimbohh/JarJar)"

// Options configure an Init run. Cfg is the base configuration (data dir, server
// dir, listen addr, claude, pipeline); Init finalizes the minecraft section from
// the provision result and writes it to ConfigPath.
type Options struct {
	Cfg         config.Config
	ConfigPath  string // where the finalized jarjard.toml is written
	Description string // the owner's plain-English pack request

	// provision knobs (not part of Config)
	MemoryMB    int
	ServerPort  int
	JavaPath    string
	ServiceUser string

	// InstallSystemd writes the unit under /etc and enables it (needs root).
	// When false, Init still provisions and builds everything and returns the
	// unit contents for the operator to install manually.
	InstallSystemd bool
	// Boot starts the service and waits for a healthy boot (implies a running
	// systemd unit, so it is only honored when InstallSystemd is set).
	Boot bool

	Log *slog.Logger
}

// Result reports what Init produced.
type Result struct {
	Plan                GenesisPlan
	Version             int
	InviteCode          string
	RconAddr            string
	SystemdUnitName     string
	SystemdUnitPath     string // set when the unit was installed under /etc
	SystemdUnitContents string
	StartScript         string
	Booted              bool
	Config              config.Config
}

// Init runs the whole zero-to-server pipeline. It is not idempotent: it refuses
// to run over an already-initialized pack.
func Init(ctx context.Context, o Options) (Result, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	log := o.Log

	cfg := o.Cfg
	if cfg.Server.DataDir == "" {
		return Result{}, fmt.Errorf("init: server.data_dir is required")
	}
	if cfg.Minecraft.ServerDir == "" {
		return Result{}, fmt.Errorf("init: minecraft.server_dir is required")
	}

	pk := pack.New(cfg.Server.DataDir)
	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "db.sqlite"))
	if err != nil {
		return Result{}, fmt.Errorf("init: open store: %w", err)
	}
	defer st.Close()

	if pk.Initialized(ctx) {
		return Result{}, fmt.Errorf("init: a pack is already initialized in %s; refusing to re-init", cfg.Server.DataDir)
	}

	// 1. Genesis — the AI decides mc/loader and existing-pack-vs-scratch.
	log.Info("genesis: planning pack from description")
	plan, err := Genesis(ctx, cfg, o.Description)
	if err != nil {
		return Result{}, err
	}
	log.Info("genesis: plan ready", "pack", plan.PackName, "mc", plan.MCVersion,
		"loader", plan.Loader.ID, "source", plan.Source.Type)

	// 2. Provision the server (loader install, EULA, server.properties, unit).
	log.Info("provision: installing server", "dir", cfg.Minecraft.ServerDir)
	prov, err := provision.Provision(ctx, provision.Options{
		MCVersion:   plan.MCVersion,
		Loader:      provision.Loader{ID: plan.Loader.ID, Version: plan.Loader.Version},
		ServerDir:   cfg.Minecraft.ServerDir,
		JavaPath:    o.JavaPath,
		MemoryMB:    o.MemoryMB,
		ServerPort:  o.ServerPort,
		RconAddr:    cfg.Minecraft.RconAddr,
		ServiceUser: o.ServiceUser,
		Log:         log,
	})
	if err != nil {
		return Result{}, fmt.Errorf("init: provision: %w", err)
	}

	// Finalize the minecraft config section from the provision result and
	// persist the config so the daemon (and operators) can read it.
	cfg.Minecraft.Control = "systemd"
	cfg.Minecraft.SystemdUnit = prov.SystemdUnitName
	cfg.Minecraft.RconAddr = prov.RconAddr
	cfg.Minecraft.RconPassword = prov.RconPassword
	if err := writeConfig(o.ConfigPath, cfg); err != nil {
		return Result{}, fmt.Errorf("init: write config %s: %w", o.ConfigPath, err)
	}
	log.Info("wrote config", "path", o.ConfigPath)

	// 3. Build pack version 1.
	reg := modtool.New(cfg.Pipeline.AllowCurseForge, cfg.Pipeline.CurseForgeAPIKey, userAgent)
	switch plan.Source.Type {
	case "existing_pack":
		if err := buildFromExisting(ctx, cfg, pk, st, reg, plan); err != nil {
			return Result{}, fmt.Errorf("init: import existing pack: %w", err)
		}
	case "scratch":
		if err := buildFromScratch(ctx, cfg, pk, st, reg, plan); err != nil {
			return Result{}, fmt.Errorf("init: build pack from scratch: %w", err)
		}
	default:
		return Result{}, fmt.Errorf("init: unknown source type %q", plan.Source.Type)
	}
	log.Info("pack version 1 published", "pack", plan.PackName)

	// 4. Seed the server dir with version 1's server-side files.
	m, err := pk.LoadManifest(1)
	if err != nil {
		return Result{}, fmt.Errorf("init: load manifest 1: %w", err)
	}
	if err := seedServerDir(pk, cfg.Minecraft.ServerDir, m); err != nil {
		return Result{}, fmt.Errorf("init: seed server dir: %w", err)
	}
	log.Info("seeded server dir with server-side files", "dir", cfg.Minecraft.ServerDir)

	res := Result{
		Plan:                plan,
		Version:             1,
		RconAddr:            prov.RconAddr,
		SystemdUnitName:     prov.SystemdUnitName,
		SystemdUnitContents: prov.SystemdUnitContents,
		StartScript:         prov.StartScript,
		Config:              cfg,
	}

	// 5. Install + boot the systemd service (needs root).
	if o.InstallSystemd {
		unitPath := filepath.Join("/etc/systemd/system", prov.SystemdUnitName)
		if err := installSystemdUnit(ctx, unitPath, prov.SystemdUnitContents); err != nil {
			return res, fmt.Errorf("init: install systemd unit: %w", err)
		}
		res.SystemdUnitPath = unitPath
		log.Info("installed systemd unit", "path", unitPath)

		if o.Boot {
			mc := mcserver.New(cfg.Minecraft)
			if err := mc.Start(ctx); err != nil {
				return res, fmt.Errorf("init: start server: %w", err)
			}
			log.Info("server starting; waiting for healthy boot")
			if err := mc.WaitHealthy(ctx); err != nil {
				return res, fmt.Errorf("init: server did not become healthy: %w", err)
			}
			res.Booted = true
			log.Info("server is up and healthy")
		}
	}

	// 6. Mint the first invite (admin), for the owner's own client.
	inv, err := st.CreateInvite(ctx, id.NewInviteCode(), store.RoleAdmin)
	if err != nil {
		return res, fmt.Errorf("init: create invite: %w", err)
	}
	res.InviteCode = inv.Code
	return res, nil
}

// buildFromExisting resolves the chosen pack version's archive, downloads it, and
// imports it as version 1 via the standard importer.
func buildFromExisting(ctx context.Context, cfg config.Config, pk *pack.Pack, st *store.Store, reg *modtool.Registry, plan GenesisPlan) error {
	v, err := reg.GetVersion(ctx, plan.Source.Platform, plan.Source.ProjectID, plan.Source.VersionID)
	if err != nil {
		return fmt.Errorf("resolve pack version %s: %w", plan.Source.VersionID, err)
	}
	if v.DownloadURL == "" {
		return fmt.Errorf("pack version %s has no download URL", plan.Source.VersionID)
	}
	expected, algo := v.SHA512, "sha512"
	if plan.Source.Platform == modtool.PlatformCurseForge {
		expected, algo = v.SHA1, "sha1"
	}
	tmp, _, err := downloadVerified(ctx, v.DownloadURL, expected, algo)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return importer.Import(ctx, cfg, pk, st, tmp, plan.PackName)
}

// buildFromScratch assembles a fresh pack: init the repo, write meta, download
// every chosen mod into the blob store, write the lock, and build version 1.
func buildFromScratch(ctx context.Context, cfg config.Config, pk *pack.Pack, st *store.Store, reg *modtool.Registry, plan GenesisPlan) error {
	repo := pk.RepoDir()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return err
	}
	if err := pack.NewGit(repo).Init(ctx); err != nil {
		return err
	}

	meta := pack.PackMeta{
		SchemaVersion: 1,
		Name:          plan.PackName,
		MCVersion:     plan.MCVersion,
		Loader:        pack.Loader{ID: plan.Loader.ID, Version: plan.Loader.Version},
		SideOverrides: map[string]string{},
	}
	if err := pack.WriteMetaTo(repo, meta); err != nil {
		return err
	}

	lock := pack.ModsLock{SchemaVersion: 1}
	for _, gm := range plan.Source.Mods {
		v, err := reg.GetVersion(ctx, gm.Platform, gm.ProjectID, gm.VersionID)
		if err != nil {
			return fmt.Errorf("resolve %s version %s: %w", gm.Platform, gm.VersionID, err)
		}
		if v.DownloadURL == "" {
			return fmt.Errorf("mod version %s has no download URL", gm.VersionID)
		}
		expected, algo := v.SHA512, "sha512"
		if gm.Platform == modtool.PlatformCurseForge {
			expected, algo = v.SHA1, "sha1"
		}
		tmp, size, err := downloadVerified(ctx, v.DownloadURL, expected, algo)
		if err != nil {
			return err
		}
		sha, _, err := pk.PutBlobFile(tmp)
		os.Remove(tmp)
		if err != nil {
			return err
		}
		versionID, fileID := v.VersionID, ""
		if gm.Platform == modtool.PlatformCurseForge {
			versionID, fileID = "", v.VersionID
		}
		lock.Mods = append(lock.Mods, pack.LockEntry{
			Path:   "mods/" + v.FileName,
			SHA256: sha,
			Size:   size,
			Side:   projectSide(ctx, reg, gm.Platform, gm.ProjectID),
			Source: pack.LockSource{
				Platform:    gm.Platform,
				ProjectID:   gm.ProjectID,
				ProjectSlug: gm.ProjectSlug,
				VersionID:   versionID,
				FileID:      fileID,
				DownloadURL: v.DownloadURL,
			},
		})
	}
	if err := pack.WriteLockTo(repo, lock); err != nil {
		return err
	}

	m, commit, err := pk.BuildVersion(ctx, pack.BuildInput{
		Worktree:  repo,
		Number:    1,
		Summary:   fmt.Sprintf("Created pack %q from scratch (%d mods).", plan.PackName, len(lock.Mods)),
		Changelog: []pack.ChangelogEntry{{Kind: "other", Text: "Initial pack (genesis)"}},
		CommitMsg: "genesis: version 1",
		Now:       time.Now(),
	})
	if err != nil {
		return fmt.Errorf("build version 1: %w", err)
	}
	if err := st.CreateVersion(ctx, store.Version{
		Number:       1,
		CreatedAt:    time.Now().UTC(),
		GitCommit:    commit,
		Summary:      m.Version.Summary,
		Status:       "published",
		ManifestPath: filepath.Join(cfg.Server.DataDir, "manifests", "1.json"),
	}); err != nil {
		return err
	}
	return pk.RefreshRetained(ctx, st, 10)
}

// projectSide classifies a project as client/server/both from registry metadata,
// mirroring worker.classifyProjectSide (unknown → both).
func projectSide(ctx context.Context, reg *modtool.Registry, platform, projectID string) string {
	p, err := reg.Project(ctx, platform, projectID)
	if err != nil {
		return pack.SideBoth
	}
	if p.ServerSide == "unsupported" {
		return pack.SideClient
	}
	if p.ClientSide == "unsupported" {
		return pack.SideServer
	}
	return pack.SideBoth
}

// serverManagedPrefixes are the paths the daemon writes into server_dir (matches
// worker.serverManagedPrefixes).
var serverManagedPrefixes = []string{"mods/", "config/", "defaultconfigs/", "kubejs/", "scripts/", "resourcepacks/", "shaderpacks/", "datapacks/"}

// seedServerDir copies version 1's server-side files (side server/both) from the
// blob store into the live server dir so the freshly provisioned server boots
// with the pack already in place.
func seedServerDir(pk *pack.Pack, serverDir string, m pack.Manifest) error {
	for _, f := range m.Files {
		if f.Side != pack.SideServer && f.Side != pack.SideBoth {
			continue
		}
		if !managedServerPath(f.Path) {
			continue
		}
		dst := filepath.Join(serverDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(pk.BlobPath(f.SHA256), dst); err != nil {
			return fmt.Errorf("seed %s: %w", f.Path, err)
		}
	}
	return nil
}

func managedServerPath(p string) bool {
	for _, pre := range serverManagedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// installSystemdUnit writes the unit file and reloads+enables it via systemctl.
func installSystemdUnit(ctx context.Context, unitPath, contents string) error {
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(contents), 0o644); err != nil {
		return err
	}
	if err := runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return runSystemctl(ctx, "enable", filepath.Base(unitPath))
}

func runSystemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeConfig serializes cfg to path as TOML, creating parent dirs.
func writeConfig(path string, cfg config.Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// downloadVerified fetches url to a temp file, verifying the expected hash
// (algo "sha512" or "sha1"), and returns the temp path and byte count.
func downloadVerified(ctx context.Context, url, expected, algo string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "jarjar-dl-*")
	if err != nil {
		return "", 0, err
	}
	h := newHash(algo)
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	if expected != "" {
		got := hexSum(h)
		if !strings.EqualFold(got, expected) {
			os.Remove(tmp.Name())
			return "", 0, fmt.Errorf("hash mismatch for %s (%s): got %s want %s", url, algo, got, expected)
		}
	}
	return tmp.Name(), n, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

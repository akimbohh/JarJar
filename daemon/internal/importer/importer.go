// Package importer ingests an existing modpack (Modrinth .mrpack or a
// CurseForge export zip) into a fresh pack repo as version 1, per PIPELINE.md §9.
// From version 1 onward, everything is diffs driven by the request pipeline.
package importer

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// Import ingests srcPath into pk (rooted at cfg data dir) and records version 1
// in st. It refuses to run if a pack is already initialized.
func Import(ctx context.Context, cfg config.Config, pk *pack.Pack, st *store.Store, srcPath, packName string) error {
	if pk.Initialized(ctx) {
		return fmt.Errorf("pack already initialized; refusing to import over it")
	}

	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open %s as zip: %w", srcPath, err)
	}
	defer zr.Close()

	// Prepare an empty pack repo (git init + .gitignore) then stage into it.
	repo := pk.RepoDir()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return err
	}
	g := pack.NewGit(repo)
	if err := g.Init(ctx); err != nil {
		return err
	}

	var lock pack.ModsLock
	lock.SchemaVersion = 1
	var meta pack.PackMeta

	switch {
	case hasFile(zr, "modrinth.index.json"):
		meta, lock, err = importMrpack(ctx, zr, pk, repo)
	case hasFile(zr, "manifest.json"):
		meta, lock, err = importCurseforge(ctx, cfg, zr, pk, repo)
	default:
		return fmt.Errorf("unrecognized pack: no modrinth.index.json or manifest.json in zip")
	}
	if err != nil {
		return err
	}
	if packName != "" {
		meta.Name = packName
	}
	if meta.Name == "" {
		meta.Name = strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	}
	meta.SchemaVersion = 1
	if meta.SideOverrides == nil {
		meta.SideOverrides = map[string]string{}
	}

	if err := pack.WriteMetaTo(repo, meta); err != nil {
		return err
	}
	if err := pack.WriteLockTo(repo, lock); err != nil {
		return err
	}

	// Build version 1.
	m, commit, err := pk.BuildVersion(ctx, pack.BuildInput{
		Worktree:  repo,
		Number:    1,
		RequestID: nil,
		Summary:   fmt.Sprintf("Imported pack %q (%d mods).", meta.Name, len(lock.Mods)),
		Changelog: []pack.ChangelogEntry{{Kind: "other", Text: "Initial import"}},
		CommitMsg: "import: version 1",
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
	if err := pk.RefreshRetained(ctx, st, 10); err != nil {
		return err
	}
	return nil
}

func hasFile(zr *zip.ReadCloser, name string) bool {
	for _, f := range zr.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

func readZipFile(zr *zip.ReadCloser, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("file %s not found in zip", name)
}

// copyOverrides copies overrides/<...> and side-specific override dirs from the
// zip into the pack repo tree, recording side_overrides for side-specific ones.
func copyOverrides(zr *zip.ReadCloser, repo string, sideOverrides map[string]string) error {
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		var rel, side string
		switch {
		case strings.HasPrefix(f.Name, "overrides/"):
			rel = strings.TrimPrefix(f.Name, "overrides/")
		case strings.HasPrefix(f.Name, "client-overrides/"):
			rel, side = strings.TrimPrefix(f.Name, "client-overrides/"), pack.SideClient
		case strings.HasPrefix(f.Name, "server-overrides/"):
			rel, side = strings.TrimPrefix(f.Name, "server-overrides/"), pack.SideServer
		default:
			continue
		}
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		dst := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		if side != "" {
			sideOverrides[filepath.ToSlash(rel)] = side
		}
	}
	return nil
}

// download fetches url to a temp file, verifying the expected hash, and returns
// the temp path. algo is "sha512" or "sha1".
func download(ctx context.Context, url, expected, algo string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "jarjar/dev (github.com/akimbohh/JarJar)")
	client := &http.Client{Timeout: 5 * time.Minute}
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
	var h hash.Hash
	switch algo {
	case "sha512":
		h = sha512.New()
	default:
		h = sha1.New()
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	if expected != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, expected) {
			os.Remove(tmp.Name())
			return "", 0, fmt.Errorf("hash mismatch for %s (%s): got %s want %s", url, algo, got, expected)
		}
	}
	return tmp.Name(), n, nil
}

// modrinth env side mapping.
func sideFromEnv(clientEnv, serverEnv string) string {
	if serverEnv == "unsupported" {
		return pack.SideClient
	}
	if clientEnv == "unsupported" {
		return pack.SideServer
	}
	return pack.SideBoth
}

// used to satisfy modtool import in curseforge path; keep referenced.
var _ = modtool.PlatformCurseForge

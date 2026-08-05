package importer

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/akimbohh/jarjar/daemon/internal/config"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// cfManifest is the CurseForge export manifest.json.
type cfManifest struct {
	Minecraft struct {
		Version    string `json:"version"`
		ModLoaders []struct {
			ID      string `json:"id"` // e.g. "neoforge-21.1.77"
			Primary bool   `json:"primary"`
		} `json:"modLoaders"`
	} `json:"minecraft"`
	Name  string `json:"name"`
	Files []struct {
		ProjectID int  `json:"projectID"`
		FileID    int  `json:"fileID"`
		Required  bool `json:"required"`
	} `json:"files"`
	Overrides string `json:"overrides"`
}

func importCurseforge(ctx context.Context, cfg config.Config, zr *zip.ReadCloser, pk *pack.Pack, repo string) (pack.PackMeta, pack.ModsLock, error) {
	if !cfg.Pipeline.AllowCurseForge {
		return pack.PackMeta{}, pack.ModsLock{}, fmt.Errorf("this is a CurseForge pack but pipeline.allow_curseforge is false")
	}
	raw, err := readZipFile(zr, "manifest.json")
	if err != nil {
		return pack.PackMeta{}, pack.ModsLock{}, err
	}
	var man cfManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return pack.PackMeta{}, pack.ModsLock{}, fmt.Errorf("parse manifest.json: %w", err)
	}

	meta := pack.PackMeta{Name: man.Name, MCVersion: man.Minecraft.Version, SideOverrides: map[string]string{}}
	meta.Loader = loaderFromCfManifest(man)
	if meta.Loader.ID == "" {
		return meta, pack.ModsLock{}, fmt.Errorf("could not determine loader from CurseForge manifest")
	}

	reg := modtool.New(true, cfg.Pipeline.CurseForgeAPIKey, "jarjar/dev (github.com/akimbohh/JarJar)")
	lock := pack.ModsLock{SchemaVersion: 1}
	for _, f := range man.Files {
		projectID := strconv.Itoa(f.ProjectID)
		fileID := strconv.Itoa(f.FileID)
		v, err := reg.GetVersion(ctx, modtool.PlatformCurseForge, projectID, fileID)
		if err != nil {
			return meta, lock, fmt.Errorf("resolve CF file %s/%s: %w", projectID, fileID, err)
		}
		if v.DownloadURL == "" {
			return meta, lock, fmt.Errorf("CF file %s/%s has no downloadUrl (project may disallow third-party downloads)", projectID, fileID)
		}
		tmp, _, err := download(ctx, v.DownloadURL, v.SHA1, "sha1")
		if err != nil {
			return meta, lock, err
		}
		sha, size, err := pk.PutBlobFile(tmp)
		os.Remove(tmp)
		if err != nil {
			return meta, lock, err
		}
		path := "mods/" + v.FileName
		lock.Mods = append(lock.Mods, pack.LockEntry{
			Path:   path,
			SHA256: sha,
			Size:   size,
			Side:   pack.SideBoth, // CurseForge exposes no side metadata
			Source: pack.LockSource{
				Platform:    modtool.PlatformCurseForge,
				ProjectID:   projectID,
				FileID:      fileID,
				DownloadURL: v.DownloadURL,
			},
		})
	}

	if err := copyOverrides(zr, repo, meta.SideOverrides); err != nil {
		return meta, lock, err
	}
	return meta, lock, nil
}

func loaderFromCfManifest(man cfManifest) pack.Loader {
	for _, ml := range man.Minecraft.ModLoaders {
		id := ml.ID
		// id is like "neoforge-21.1.77"; split on the first '-'.
		idx := strings.Index(id, "-")
		if idx < 0 {
			continue
		}
		name, ver := id[:idx], id[idx+1:]
		switch name {
		case "fabric":
			return pack.Loader{ID: "fabric", Version: ver}
		case "quilt":
			return pack.Loader{ID: "quilt", Version: ver}
		case "neoforge":
			return pack.Loader{ID: "neoforge", Version: ver}
		case "forge":
			return pack.Loader{ID: "forge", Version: ver}
		}
	}
	return pack.Loader{}
}

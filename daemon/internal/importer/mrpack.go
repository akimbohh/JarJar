package importer

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// mrIndex is modrinth.index.json.
type mrIndex struct {
	FormatVersion int               `json:"formatVersion"`
	Name          string            `json:"name"`
	VersionID     string            `json:"versionId"`
	Dependencies  map[string]string `json:"dependencies"`
	Files         []mrIndexFile     `json:"files"`
}

type mrIndexFile struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       *mrIndexEnv       `json:"env"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}

type mrIndexEnv struct {
	Client string `json:"client"`
	Server string `json:"server"`
}

func importMrpack(ctx context.Context, zr *zip.ReadCloser, pk *pack.Pack, repo string) (pack.PackMeta, pack.ModsLock, error) {
	raw, err := readZipFile(zr, "modrinth.index.json")
	if err != nil {
		return pack.PackMeta{}, pack.ModsLock{}, err
	}
	var idx mrIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return pack.PackMeta{}, pack.ModsLock{}, fmt.Errorf("parse modrinth.index.json: %w", err)
	}

	meta := pack.PackMeta{Name: idx.Name, SideOverrides: map[string]string{}}
	meta.MCVersion = idx.Dependencies["minecraft"]
	meta.Loader = loaderFromMrDeps(idx.Dependencies)
	if meta.Loader.ID == "" {
		return meta, pack.ModsLock{}, fmt.Errorf("could not determine loader from mrpack dependencies")
	}

	lock := pack.ModsLock{SchemaVersion: 1}
	for _, f := range idx.Files {
		if len(f.Downloads) == 0 {
			return meta, lock, fmt.Errorf("mrpack file %s has no download URLs", f.Path)
		}
		expected, algo := f.Hashes["sha512"], "sha512"
		if expected == "" {
			expected, algo = f.Hashes["sha1"], "sha1"
		}
		tmp, _, err := download(ctx, f.Downloads[0], expected, algo)
		if err != nil {
			return meta, lock, err
		}
		sha, size, err := pk.PutBlobFile(tmp)
		os.Remove(tmp)
		if err != nil {
			return meta, lock, err
		}
		side := pack.SideBoth
		if f.Env != nil {
			side = sideFromEnv(f.Env.Client, f.Env.Server)
		}
		path := filepath.ToSlash(f.Path)
		lock.Mods = append(lock.Mods, pack.LockEntry{
			Path:   path,
			SHA256: sha,
			Size:   size,
			Side:   side,
			Source: pack.LockSource{
				Platform:    modtool.PlatformManual, // mrpack files carry URLs, not registry ids
				DownloadURL: f.Downloads[0],
			},
		})
		// Non-mod files (rare in mrpack) also get side overrides so the manifest
		// reflects env; only record when not "both".
		if side != pack.SideBoth && !strings.HasPrefix(path, "mods/") {
			meta.SideOverrides[path] = side
		}
	}

	if err := copyOverrides(zr, repo, meta.SideOverrides); err != nil {
		return meta, lock, err
	}
	return meta, lock, nil
}

func loaderFromMrDeps(deps map[string]string) pack.Loader {
	for key, id := range map[string]string{
		"fabric-loader": "fabric",
		"quilt-loader":  "quilt",
		"neoforge":      "neoforge",
		"forge":         "forge",
	} {
		if v, ok := deps[key]; ok {
			return pack.Loader{ID: id, Version: v}
		}
	}
	return pack.Loader{}
}

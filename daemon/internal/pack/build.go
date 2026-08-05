package pack

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// managedDirs are the committed top-level directories whose files are part of a
// version (jars excepted — they live in the blob store, referenced by the lock).
var managedDirs = []string{"config", "defaultconfigs", "kubejs", "scripts", "resourcepacks", "shaderpacks", "datapacks"}

// BuildInput carries everything BuildVersion needs beyond the on-disk worktree.
type BuildInput struct {
	Worktree  string
	Number    int
	RequestID *string
	Summary   string
	Changelog []ChangelogEntry
	CommitMsg string
	Now       time.Time
}

// BuildVersion commits the worktree, integrates it into the pack repo as version
// Number (tag v<Number>), and writes manifests/<Number>.json. It returns the
// manifest and the git commit SHA. Config/script files are stored as blobs;
// mod jars are expected to already be in the blob store (materialize stage).
func (p *Pack) BuildVersion(ctx context.Context, in BuildInput) (Manifest, string, error) {
	commit, err := commitAllInWorktree(ctx, in.Worktree, in.CommitMsg)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("commit worktree: %w", err)
	}
	if err := p.git.fastForwardMain(ctx, commit, fmt.Sprintf("v%d", in.Number)); err != nil {
		return Manifest{}, "", fmt.Errorf("integrate commit: %w", err)
	}

	meta, ok, err := p.MetaFull(ctx)
	if err != nil || !ok {
		return Manifest{}, "", fmt.Errorf("read pack meta: %w (ok=%v)", err, ok)
	}
	lock, err := p.ReadLock(ctx)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("read lock: %w", err)
	}

	files, err := p.assembleFiles(meta, lock)
	if err != nil {
		return Manifest{}, "", err
	}

	m := Manifest{
		SchemaVersion: 1,
		Pack:          ManifestPack{Name: meta.Name, MCVersion: meta.MCVersion, Loader: meta.Loader},
		Version: ManifestVersion{
			Number:    in.Number,
			CreatedAt: in.Now.UTC().Format(time.RFC3339),
			RequestID: in.RequestID,
			GitCommit: commit,
			Summary:   in.Summary,
			Changelog: in.Changelog,
		},
		Files: files,
	}
	if m.Version.Changelog == nil {
		m.Version.Changelog = []ChangelogEntry{}
	}
	if err := p.writeManifest(in.Number, m); err != nil {
		return Manifest{}, "", err
	}
	return m, commit, nil
}

// assembleFiles builds the manifest file list: mod entries from the lock (their
// jars are in the blob store) plus every committed file under managedDirs
// (stored as blobs here).
func (p *Pack) assembleFiles(meta PackMeta, lock ModsLock) ([]ManifestFile, error) {
	var files []ManifestFile

	for _, m := range lock.Mods {
		side := classifySide(meta, m.Path, m.Side)
		files = append(files, ManifestFile{
			Path: m.Path, SHA256: m.SHA256, Size: m.Size, Side: side, Kind: "mod",
		})
	}

	for _, dir := range managedDirs {
		root := filepath.Join(p.repoDir, dir)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(p.repoDir, path)
			rel = filepath.ToSlash(rel)
			sha, size, perr := p.blobs.PutFile(path)
			if perr != nil {
				return perr
			}
			files = append(files, ManifestFile{
				Path: rel, SHA256: sha, Size: size,
				Side: classifySide(meta, rel, ""), Kind: kindForPath(rel),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// classifySide applies the precedence from DATA-CONTRACTS.md §4: overrides win,
// then a supplied registry side, else both.
func classifySide(meta PackMeta, path, registrySide string) string {
	if meta.SideOverrides != nil {
		if s, ok := meta.SideOverrides[path]; ok {
			return s
		}
	}
	switch registrySide {
	case SideClient, SideServer, SideBoth:
		return registrySide
	}
	return SideBoth
}

func kindForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "mods/"):
		return "mod"
	case strings.HasPrefix(path, "config/"), strings.HasPrefix(path, "defaultconfigs/"):
		return "config"
	case strings.HasPrefix(path, "kubejs/"), strings.HasPrefix(path, "scripts/"):
		return "script"
	case strings.HasPrefix(path, "resourcepacks/"), strings.HasPrefix(path, "shaderpacks/"), strings.HasPrefix(path, "datapacks/"):
		return "resource"
	default:
		return "other"
	}
}

// ---- meta & lock read/write ----

func (p *Pack) ReadLock(ctx context.Context) (ModsLock, error) {
	return readLockAt(p.repoDir)
}

func readLockAt(dir string) (ModsLock, error) {
	buf, err := os.ReadFile(filepath.Join(dir, "mods.lock.json"))
	if os.IsNotExist(err) {
		return ModsLock{SchemaVersion: 1}, nil
	} else if err != nil {
		return ModsLock{}, err
	}
	var l ModsLock
	if err := json.Unmarshal(buf, &l); err != nil {
		return ModsLock{}, err
	}
	return l, nil
}

func writeLockAt(dir string, lock ModsLock) error {
	lock.SchemaVersion = 1
	sort.Slice(lock.Mods, func(i, j int) bool { return lock.Mods[i].Path < lock.Mods[j].Path })
	buf, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "mods.lock.json"), buf, 0o644)
}

func readMetaAt(dir string) (PackMeta, error) {
	buf, err := os.ReadFile(filepath.Join(dir, "jarjar.pack.json"))
	if err != nil {
		return PackMeta{}, err
	}
	var m PackMeta
	if err := json.Unmarshal(buf, &m); err != nil {
		return PackMeta{}, err
	}
	return m, nil
}

func writeMetaAt(dir string, meta PackMeta) error {
	meta.SchemaVersion = 1
	buf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "jarjar.pack.json"), buf, 0o644)
}

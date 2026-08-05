package pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// Pack manages the pack repo, blob store, and manifests rooted at dataDir.
type Pack struct {
	dataDir     string
	repoDir     string // <data>/pack
	worktreeDir string // <data>/worktrees
	manifestDir string // <data>/manifests
	blobs       *blobStore
	git         gitRepo
	retained    retainedSet
}

// New constructs a Pack over dataDir. It does not create the repo (use Init or
// Import for that).
func New(dataDir string) *Pack {
	repo := filepath.Join(dataDir, "pack")
	return &Pack{
		dataDir:     dataDir,
		repoDir:     repo,
		worktreeDir: filepath.Join(dataDir, "worktrees"),
		manifestDir: filepath.Join(dataDir, "manifests"),
		blobs:       newBlobStore(dataDir),
		git:         gitRepo{dir: repo},
		retained:    newRetainedSet(),
	}
}

// RepoDir returns the pack repo path.
func (p *Pack) RepoDir() string { return p.repoDir }

// Initialized reports whether a pack has been imported (repo has a commit).
func (p *Pack) Initialized(ctx context.Context) bool { return p.git.hasCommit(ctx) }

// Meta reads jarjar.pack.json from the current pack repo. ok=false if the pack
// has not been initialized yet.
func (p *Pack) MetaFull(ctx context.Context) (PackMeta, bool, error) {
	path := filepath.Join(p.repoDir, "jarjar.pack.json")
	buf, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PackMeta{}, false, nil
	} else if err != nil {
		return PackMeta{}, false, err
	}
	var m PackMeta
	if err := json.Unmarshal(buf, &m); err != nil {
		return PackMeta{}, false, err
	}
	return m, true, nil
}

// ---- api.Pack implementation ----

// Meta returns the api-shaped pack metadata.
func (p *Pack) Meta(ctx context.Context) (name, mcVersion, loaderID, loaderVersion string, ok bool, err error) {
	m, ok, err := p.MetaFull(ctx)
	if err != nil || !ok {
		return "", "", "", "", ok, err
	}
	return m.Name, m.MCVersion, m.Loader.ID, m.Loader.Version, true, nil
}

// ManifestBytes returns the raw manifest JSON for version n.
func (p *Pack) ManifestBytes(ctx context.Context, n int) ([]byte, error) {
	buf, err := os.ReadFile(p.manifestPath(n))
	if errors.Is(err, os.ErrNotExist) {
		return nil, store.ErrNotFound
	}
	return buf, err
}

// Changelog returns the changelog of version n (empty if unknown).
func (p *Pack) Changelog(ctx context.Context, n int) ([]ChangelogEntry, error) {
	m, err := p.LoadManifest(n)
	if err != nil {
		return nil, err
	}
	return m.Version.Changelog, nil
}

// OpenBlob opens a blob for range-capable serving.
func (p *Pack) OpenBlob(sha string) (io.ReadSeekCloser, int64, error) {
	f, size, err := p.blobs.Open(sha)
	if err != nil {
		return nil, 0, err
	}
	return f, size, nil
}

// BlobRetained reports whether sha is referenced by a retained manifest.
func (p *Pack) BlobRetained(sha string) bool { return p.retained.has(sha) }

// ---- manifest helpers ----

func (p *Pack) manifestPath(n int) string {
	return filepath.Join(p.manifestDir, strconv.Itoa(n)+".json")
}

// LoadManifest reads and parses manifest n.
func (p *Pack) LoadManifest(n int) (Manifest, error) {
	buf, err := os.ReadFile(p.manifestPath(n))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, store.ErrNotFound
	} else if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(buf, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %d: %w", n, err)
	}
	return m, nil
}

// writeManifest persists a manifest atomically.
func (p *Pack) writeManifest(n int, m Manifest) error {
	if err := os.MkdirAll(p.manifestDir, 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.manifestPath(n) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.manifestPath(n))
}

// RefreshRetained recomputes the retained-blob set from the newest keepN
// manifests. Call after publish and on startup.
func (p *Pack) RefreshRetained(ctx context.Context, st *store.Store, keepN int) error {
	versions, err := st.ListVersions(ctx, keepN, 0)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, v := range versions {
		m, err := p.LoadManifest(v.Number)
		if err != nil {
			continue
		}
		for _, f := range m.Files {
			set[f.SHA256] = true
		}
	}
	p.retained.replace(set)
	return nil
}

// RunGC deletes unreferenced blobs, keeping those in the retained set.
func (p *Pack) RunGC() (int, error) {
	return p.blobs.GC(p.retained.snapshot())
}

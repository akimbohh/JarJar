package pack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBlobStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bs := newBlobStore(dir)

	sha, size, err := bs.PutBytes([]byte("hello jarjar"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if size != 12 {
		t.Fatalf("size = %d", size)
	}
	if !bs.Exists(sha) {
		t.Fatal("blob should exist")
	}
	if err := bs.Verify(sha); err != nil {
		t.Fatalf("verify: %v", err)
	}
	f, sz, err := bs.Open(sha)
	if err != nil || sz != 12 {
		t.Fatalf("open: sz=%d err=%v", sz, err)
	}
	f.Close()

	// GC keeps referenced, removes the rest.
	other, _, _ := bs.PutBytes([]byte("orphan"))
	removed, err := bs.GC(map[string]bool{sha: true})
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if bs.Exists(other) {
		t.Fatal("orphan should be gone")
	}
	if !bs.Exists(sha) {
		t.Fatal("referenced blob should remain")
	}
}

func TestBuildVersionManifest(t *testing.T) {
	dir := t.TempDir()
	pk := New(dir)
	ctx := context.Background()

	repo := pk.RepoDir()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewGit(repo).Init(ctx); err != nil {
		t.Fatalf("git init: %v", err)
	}

	meta := PackMeta{
		SchemaVersion: 1, Name: "Test Pack", MCVersion: "1.21.1",
		Loader:        Loader{ID: "fabric", Version: "0.16.0"},
		SideOverrides: map[string]string{"config/client-only.json": SideClient},
	}
	if err := WriteMetaTo(repo, meta); err != nil {
		t.Fatal(err)
	}
	// One mod already in the lock (jar blob must exist).
	jarSha, jarSize, _ := pk.PutBlobBytes([]byte("fake jar bytes"))
	lock := ModsLock{SchemaVersion: 1, Mods: []LockEntry{{
		Path: "mods/example.jar", SHA256: jarSha, Size: jarSize, Side: SideBoth,
		Source: LockSource{Platform: "modrinth", ProjectID: "abc", VersionID: "v1"},
	}}}
	if err := WriteLockTo(repo, lock); err != nil {
		t.Fatal(err)
	}
	// Config files.
	os.MkdirAll(filepath.Join(repo, "config"), 0o755)
	os.WriteFile(filepath.Join(repo, "config", "settings.toml"), []byte("a = 1\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "config", "client-only.json"), []byte("{}\n"), 0o644)

	m, commit, err := pk.BuildVersion(ctx, BuildInput{
		Worktree: repo, Number: 1, Summary: "init",
		Changelog: []ChangelogEntry{{Kind: "other", Text: "Initial"}},
		CommitMsg: "v1", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(commit) != 40 {
		t.Fatalf("commit sha length = %d", len(commit))
	}

	// Expect 3 files: the mod + two configs.
	if len(m.Files) != 3 {
		t.Fatalf("files = %d, want 3: %+v", len(m.Files), m.Files)
	}
	// Files sorted by path.
	for i := 1; i < len(m.Files); i++ {
		if m.Files[i-1].Path > m.Files[i].Path {
			t.Fatal("files not sorted by path")
		}
	}
	// Side override honored.
	for _, f := range m.Files {
		if f.Path == "config/client-only.json" && f.Side != SideClient {
			t.Fatalf("side override not applied: %+v", f)
		}
	}
	// Manifest is retrievable and its blobs are present.
	loaded, err := pk.LoadManifest(1)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, f := range loaded.Files {
		if !pk.BlobExists(f.SHA256) {
			t.Fatalf("blob missing for %s", f.Path)
		}
	}
}

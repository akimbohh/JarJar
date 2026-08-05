package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// blobStore is the content-addressed store at <data>/blobs/sha256/<aa>/<hash>.
type blobStore struct {
	root string // <data>/blobs/sha256
}

func newBlobStore(dataDir string) *blobStore {
	return &blobStore{root: filepath.Join(dataDir, "blobs", "sha256")}
}

func (b *blobStore) pathFor(sha string) string {
	return filepath.Join(b.root, sha[:2], sha)
}

// Exists reports whether a blob is present.
func (b *blobStore) Exists(sha string) bool {
	_, err := os.Stat(b.pathFor(sha))
	return err == nil
}

// Open opens a blob for reading; returns the size too.
func (b *blobStore) Open(sha string) (*os.File, int64, error) {
	f, err := os.Open(b.pathFor(sha))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// PutFile hashes src, stores it under its sha256, and returns (sha, size). If a
// blob with that hash already exists it is a no-op.
func (b *blobStore) PutFile(src string) (sha string, size int64, err error) {
	f, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return b.PutReader(f)
}

// PutBytes stores a byte slice.
func (b *blobStore) PutBytes(data []byte) (sha string, size int64, err error) {
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	if b.Exists(sha) {
		return sha, int64(len(data)), nil
	}
	if err := b.writeAtomic(sha, func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); err != nil {
		return "", 0, err
	}
	return sha, int64(len(data)), nil
}

// PutReader streams r into the store. It buffers to a temp file while hashing,
// then renames into place.
func (b *blobStore) PutReader(r io.Reader) (sha string, size int64, err error) {
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(b.root, ".put-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	sha = hex.EncodeToString(h.Sum(nil))
	dst := b.pathFor(sha)
	if b.Exists(sha) {
		return sha, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", 0, err
	}
	return sha, n, nil
}

func (b *blobStore) writeAtomic(sha string, write func(io.Writer) error) error {
	dst := b.pathFor(sha)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// Verify recomputes the hash of a stored blob and checks it matches sha.
func (b *blobStore) Verify(sha string) error {
	f, _, err := b.Open(sha)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != sha {
		return fmt.Errorf("blob %s hash mismatch (got %s)", sha, got)
	}
	return nil
}

// GC deletes blobs not present in keep. Returns the number removed.
func (b *blobStore) GC(keep map[string]bool) (int, error) {
	removed := 0
	prefixes, err := os.ReadDir(b.root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, pre := range prefixes {
		if !pre.IsDir() {
			continue
		}
		dir := filepath.Join(b.root, pre.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return removed, err
		}
		for _, f := range files {
			if keep[f.Name()] {
				continue
			}
			if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

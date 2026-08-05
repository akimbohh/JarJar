package worker

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// materialize applies the plan's mod actions deterministically: download jars,
// verify registry hashes, store blobs, and rewrite mods.lock.json in the
// worktree. It re-resolves versions from the registry using the (validated)
// plan version ids so the direct and approval-resume paths behave identically.
func (d Deps) materialize(ctx context.Context, worktree string, plan Plan) error {
	meta, err := pack.ReadMetaFrom(worktree)
	if err != nil {
		return err
	}
	lock, err := pack.ReadLockFrom(worktree)
	if err != nil {
		return err
	}

	// Remove entries first (remove_mod + update_mod.from_path).
	removes := map[string]bool{}
	for _, a := range plan.Actions {
		switch a.Type {
		case ActRemoveMod:
			removes[a.Path] = true
		case ActUpdateMod:
			removes[a.FromPath] = true
		}
	}
	if len(removes) > 0 {
		kept := lock.Mods[:0]
		for _, m := range lock.Mods {
			if !removes[m.Path] {
				kept = append(kept, m)
			}
		}
		lock.Mods = kept
	}

	// Add/update entries.
	for _, a := range plan.Actions {
		if a.Type != ActAddMod && a.Type != ActUpdateMod {
			continue
		}
		v, err := d.Registry.GetVersion(ctx, a.Platform, a.ProjectID, a.VersionID)
		if err != nil {
			return fmt.Errorf("resolve %s version %s: %w", a.Platform, a.VersionID, err)
		}
		if v.DownloadURL == "" {
			return fmt.Errorf("version %s has no download URL", a.VersionID)
		}
		expected, algo := v.SHA512, "sha512"
		if a.Platform == modtool.PlatformCurseForge {
			expected, algo = v.SHA1, "sha1"
		}
		tmp, size, err := downloadVerified(ctx, v.DownloadURL, expected, algo)
		if err != nil {
			return err
		}
		sha, _, err := d.Pack.PutBlobFile(tmp)
		os.Remove(tmp)
		if err != nil {
			return err
		}
		side := d.classifyProjectSide(ctx, a.Platform, a.ProjectID)
		path := "mods/" + v.FileName
		fileID := ""
		versionID := v.VersionID
		if a.Platform == modtool.PlatformCurseForge {
			fileID = v.VersionID
			versionID = ""
		}
		lock.Mods = append(lock.Mods, pack.LockEntry{
			Path:   path,
			SHA256: sha,
			Size:   size,
			Side:   side,
			Source: pack.LockSource{
				Platform:    a.Platform,
				ProjectID:   a.ProjectID,
				ProjectSlug: a.ProjectSlug,
				VersionID:   versionID,
				FileID:      fileID,
				DownloadURL: v.DownloadURL,
			},
		})
	}

	_ = meta
	return pack.WriteLockTo(worktree, lock)
}

// downloadVerified downloads url to a temp file, verifying the expected hash.
func downloadVerified(ctx context.Context, url, expected, algo string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "jarjar/dev (github.com/akimbohh/JarJar)")
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "jarjar-jar-*")
	if err != nil {
		return "", 0, err
	}
	var h hash.Hash
	if algo == "sha512" {
		h = sha512.New()
	} else {
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
			return "", 0, fmt.Errorf("hash mismatch for %s: got %s want %s", url, got, expected)
		}
	}
	return tmp.Name(), n, nil
}

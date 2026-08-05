package pack

import (
	"context"
	"os"
	"path/filepath"
)

// WorktreePath returns the throwaway worktree path for a job id.
func (p *Pack) WorktreePath(jobID string) string {
	return filepath.Join(p.worktreeDir, "job-"+jobID)
}

// AddWorktree creates a detached worktree for a job checked out to HEAD.
func (p *Pack) AddWorktree(ctx context.Context, jobID string) (string, error) {
	if err := os.MkdirAll(p.worktreeDir, 0o755); err != nil {
		return "", err
	}
	path := p.WorktreePath(jobID)
	os.RemoveAll(path) // clear any stale leftover
	if err := p.git.addWorktree(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

// AddWorktreeFromTag creates a job worktree checked out to a version tag
// (rollback: restores that version's committed tree exactly).
func (p *Pack) AddWorktreeFromTag(ctx context.Context, jobID, tag string) (string, error) {
	if err := os.MkdirAll(p.worktreeDir, 0o755); err != nil {
		return "", err
	}
	path := p.WorktreePath(jobID)
	os.RemoveAll(path)
	if _, err := p.git.run(ctx, "worktree", "add", "--detach", path, tag); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveWorktree removes a job worktree (best effort).
func (p *Pack) RemoveWorktree(ctx context.Context, jobID string) {
	p.git.removeWorktree(ctx, p.WorktreePath(jobID))
}

// PruneWorktrees removes any leftover worktree directories (startup cleanup).
func (p *Pack) PruneWorktrees(ctx context.Context) {
	p.git.run(ctx, "worktree", "prune")
	entries, err := os.ReadDir(p.worktreeDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p.git.removeWorktree(ctx, filepath.Join(p.worktreeDir, e.Name()))
	}
}

// HeadCommit returns the current pack HEAD commit.
func (p *Pack) HeadCommit(ctx context.Context) (string, error) { return p.git.headCommit(ctx) }

// StagePending commits the current worktree state and records it under
// refs/jarjar/pending/<jobID> so it survives across worker invocations (the
// approval gate). Returns the commit SHA.
func (p *Pack) StagePending(ctx context.Context, jobID, worktree, msg string) (string, error) {
	sha, err := commitAllInWorktree(ctx, worktree, msg)
	if err != nil {
		return "", err
	}
	if _, err := p.git.run(ctx, "update-ref", p.pendingRef(jobID), sha); err != nil {
		return "", err
	}
	return sha, nil
}

// AddWorktreeFromPending creates a job worktree checked out to the pending ref.
func (p *Pack) AddWorktreeFromPending(ctx context.Context, jobID string) (string, error) {
	if err := os.MkdirAll(p.worktreeDir, 0o755); err != nil {
		return "", err
	}
	path := p.WorktreePath(jobID)
	os.RemoveAll(path)
	if _, err := p.git.run(ctx, "worktree", "add", "--detach", path, p.pendingRef(jobID)); err != nil {
		return "", err
	}
	return path, nil
}

// DropPending removes a pending ref (after approval-resume or rejection).
func (p *Pack) DropPending(ctx context.Context, jobID string) {
	p.git.run(ctx, "update-ref", "-d", p.pendingRef(jobID))
}

func (p *Pack) pendingRef(jobID string) string { return "refs/jarjar/pending/" + jobID }

// ResetToTag hard-resets the pack repo (main is checked out, so reset --hard
// moves the branch and the working tree together) to a tag — used to undo a
// failed publish.
func (p *Pack) ResetToTag(ctx context.Context, tag string) error {
	_, err := p.git.run(ctx, "reset", "--hard", tag)
	return err
}

// DeleteTag removes a version tag (used when rolling back a failed publish).
func (p *Pack) DeleteTag(ctx context.Context, tag string) error {
	p.git.run(ctx, "tag", "-d", tag)
	return nil
}

// DeleteManifest removes a manifest file (failed-publish cleanup).
func (p *Pack) DeleteManifest(n int) error {
	err := os.Remove(p.manifestPath(n))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ---- blob store passthroughs ----

func (p *Pack) PutBlobFile(src string) (sha string, size int64, err error) {
	return p.blobs.PutFile(src)
}
func (p *Pack) PutBlobBytes(b []byte) (sha string, size int64, err error) { return p.blobs.PutBytes(b) }
func (p *Pack) BlobExists(sha string) bool                                { return p.blobs.Exists(sha) }
func (p *Pack) VerifyBlob(sha string) error                               { return p.blobs.Verify(sha) }
func (p *Pack) BlobPath(sha string) string                                { return p.blobs.pathFor(sha) }

// ---- lock/meta passthroughs scoped to a worktree ----

// ReadLockFrom reads mods.lock.json from an arbitrary directory (a worktree).
func ReadLockFrom(dir string) (ModsLock, error) { return readLockAt(dir) }

// WriteLockTo writes mods.lock.json into a directory (a worktree).
func WriteLockTo(dir string, lock ModsLock) error { return writeLockAt(dir, lock) }

// DiffNames returns the paths changed in a worktree relative to HEAD.
func DiffNames(ctx context.Context, worktree string) ([]string, error) {
	return diffNamesInWorktree(ctx, worktree)
}

// ReadMetaFrom reads jarjar.pack.json from an arbitrary directory.
func ReadMetaFrom(dir string) (PackMeta, error) {
	return readMetaAt(dir)
}

// WriteMetaTo writes jarjar.pack.json into a directory.
func WriteMetaTo(dir string, meta PackMeta) error { return writeMetaAt(dir, meta) }

// Git is an exported handle to a git repo for the importer's one-time init.
type Git struct{ r gitRepo }

// NewGit wraps a repo directory.
func NewGit(dir string) Git { return Git{r: gitRepo{dir: dir}} }

// Init initializes an empty pack repo (git init + default .gitignore).
func (g Git) Init(ctx context.Context) error { return g.r.initRepo(ctx) }

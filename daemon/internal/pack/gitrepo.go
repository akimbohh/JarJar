package pack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitRepo wraps the pack git repository with the subset of operations the
// pipeline needs. All commands run with a fixed identity so commits are
// reproducible regardless of host git config.
type gitRepo struct {
	dir string
}

func (g gitRepo) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=jarjar", "GIT_AUTHOR_EMAIL=jarjar@localhost",
		"GIT_COMMITTER_NAME=jarjar", "GIT_COMMITTER_EMAIL=jarjar@localhost",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// initRepo initializes an empty pack repo with the default .gitignore.
func (g gitRepo) initRepo(ctx context.Context) error {
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	if _, err := g.run(ctx, "init", "-b", "main"); err != nil {
		return err
	}
	// Jars live in the blob store; CLAUDE.md and .jarjar/ are per-job scaffolding
	// the worker writes into worktrees and must never be committed or appear in
	// the change diff.
	gitignore := filepath.Join(g.dir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("mods/*.jar\n/CLAUDE.md\n/.jarjar/\n"), 0o644); err != nil {
		return err
	}
	return nil
}

// isInitialized reports whether the repo has at least one commit.
func (g gitRepo) hasCommit(ctx context.Context) bool {
	_, err := g.run(ctx, "rev-parse", "HEAD")
	return err == nil
}

// headCommit returns the full 40-char SHA of HEAD.
func (g gitRepo) headCommit(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "HEAD")
}

// addWorktree creates a detached worktree at path checked out to HEAD.
func (g gitRepo) addWorktree(ctx context.Context, path string) error {
	_, err := g.run(ctx, "worktree", "add", "--detach", path, "HEAD")
	return err
}

// removeWorktree force-removes a worktree.
func (g gitRepo) removeWorktree(ctx context.Context, path string) error {
	_, err := g.run(ctx, "worktree", "remove", "--force", path)
	if err != nil {
		// Best effort: prune stale metadata and delete the dir.
		g.run(ctx, "worktree", "prune")
		os.RemoveAll(path)
	}
	return nil
}

// commitAll stages and commits everything in the given worktree dir. Returns
// the new commit SHA. It runs git inside the worktree so the commit lands on
// the worktree's checked-out state, then the caller integrates it.
func commitAllInWorktree(ctx context.Context, worktree, message string) (string, error) {
	g := gitRepo{dir: worktree}
	if _, err := g.run(ctx, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, "commit", "-m", message, "--allow-empty"); err != nil {
		return "", err
	}
	return g.run(ctx, "rev-parse", "HEAD")
}

// fastForwardMain integrates commit (made in a linked job worktree, a
// descendant of main) into the pack repo's checked-out main branch and tags it.
// A plain `branch -f main` is refused while main is checked out, so we
// fast-forward the working tree with merge --ff-only instead. When the commit
// was made directly on main (import path, worktree == repo dir) main already
// points at it and the merge is a no-op.
func (g gitRepo) fastForwardMain(ctx context.Context, commit, tag string) error {
	cur, err := g.run(ctx, "rev-parse", "main")
	if err == nil && cur != commit {
		if _, err := g.run(ctx, "merge", "--ff-only", commit); err != nil {
			return err
		}
	}
	if _, err := g.run(ctx, "tag", "-f", tag, commit); err != nil {
		return err
	}
	return nil
}

// diffNames returns the paths changed in the worktree relative to HEAD.
func diffNamesInWorktree(ctx context.Context, worktree string) ([]string, error) {
	g := gitRepo{dir: worktree}
	out, err := g.run(ctx, "add", "-A", "--dry-run")
	_ = out
	if err != nil {
		return nil, err
	}
	// Use status --porcelain to capture adds/mods/deletes reliably.
	st, err := g.run(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(st, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "XY path" (path may be quoted; renames "old -> new").
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		p := strings.TrimSpace(parts[1])
		if idx := strings.Index(p, " -> "); idx >= 0 {
			p = p[idx+4:]
		}
		p = strings.Trim(p, `"`)
		names = append(names, p)
	}
	return names, nil
}

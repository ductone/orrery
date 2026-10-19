package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ductone/orrery/agentproto"
)

// createIsolatedWorktree creates a git worktree at worktreePath based on the
// parent workspace HEAD, then overlays the parent's uncommitted files into it
// so the worker sees the parent's working tree. It returns an error on failure.
// The caller should remove the worktree with removeIsolatedWorktree when done.
func createIsolatedWorktree(ctx context.Context, parentWorkspace, worktreePath, branch string) error {
	if !isGitWorkspace(ctx, parentWorkspace) {
		return fmt.Errorf("not a git workspace: %s", parentWorkspace)
	}
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o700); err != nil {
		return err
	}
	add := exec.CommandContext(ctx, "git", "-C", parentWorkspace, "worktree", "add", "-q", "-b", branch, worktreePath, "HEAD")
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	// Copy parent's working files into the worktree, skipping .git and .orrery.
	_ = filepath.WalkDir(parentWorkspace, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(parentWorkspace, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			return nil
		}
		if rel == ".orrery" || strings.HasPrefix(rel, ".orrery"+string(os.PathSeparator)) {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		dst := filepath.Join(worktreePath, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return err
		}
		return nil
	})
	// Stage and commit any copied files in the worktree (ignore nothing-to-commit).
	addAll := exec.CommandContext(ctx, "git", "-C", worktreePath, "add", "-A")
	if out, err := addAll.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	commit := exec.CommandContext(ctx, "git", "-C", worktreePath, "commit", "-m", "worker base: include parent's uncommitted changes")
	if out, err := commit.CombinedOutput(); err != nil {
		// ignore "nothing to commit" errors
		if !strings.Contains(string(out), "nothing to commit") {
			return fmt.Errorf("git commit: %w: %s", err, out)
		}
	}
	return nil
}

// removeIsolatedWorktree removes the git worktree and its branch created for a job.
// It attempts to remove the worktree and delete the branch in the parent repo.
func removeIsolatedWorktree(ctx context.Context, parentWorkspace, worktreePath, branch string) error {
	// First remove the worktree entry.
	rm := exec.CommandContext(ctx, "git", "-C", parentWorkspace, "worktree", "remove", "-f", worktreePath)
	if out, err := rm.CombinedOutput(); err != nil {
		// Continue to attempt branch deletion even if removal failed.
		_ = fmt.Errorf("git worktree remove: %w: %s", err, out)
	}
	// Delete the branch reference in the parent workspace.
	del := exec.CommandContext(ctx, "git", "-C", parentWorkspace, "branch", "-D", branch)
	if out, err := del.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -D: %w: %s", err, out)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) string {
	out, _ := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out))
}

// finalizeIsolatedJob commits the worker's changes on its branch, applies them
// to the parent checkout, and removes the worktree. On a conflict the parent is
// left untouched, the branch is kept, and the conflicting files are reported.
func finalizeIsolatedJob(ctx context.Context, parentWorkspace, worktreePath, branch, base string, result agentproto.TaskResult) agentproto.TaskResult {
	if result.Result == nil {
		result.Result = map[string]any{}
	}
	result.Result["branch"] = branch
	_ = exec.CommandContext(ctx, "git", "-C", worktreePath, "add", "-A").Run()
	_ = exec.CommandContext(ctx, "git", "-C", worktreePath, "commit", "-q", "-m", "worker changes").Run()
	var commits []string
	for _, l := range strings.Split(gitOutput(ctx, worktreePath, "log", "--format=%h %s", base+"..HEAD"), "\n") {
		if l != "" {
			commits = append(commits, l)
		}
	}
	var files []string
	for _, f := range strings.Split(gitOutput(ctx, worktreePath, "diff", "--name-only", base, "HEAD"), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	result.Result["commits"] = commits
	result.Result["files"] = files
	merged := true
	if len(files) > 0 {
		patch, err := exec.CommandContext(ctx, "git", "-C", worktreePath, "diff", "--binary", base, "HEAD").Output()
		if err == nil {
			apply := exec.CommandContext(ctx, "git", "-C", parentWorkspace, "apply", "--whitespace=nowarn", "-")
			apply.Stdin = bytes.NewReader(patch)
			if out, aerr := apply.CombinedOutput(); aerr != nil {
				err = fmt.Errorf("%w: %s", aerr, out)
			}
		}
		if err != nil {
			merged = false
			result.Result["merged"] = false
			result.Result["conflicting_files"] = files
			result.Result["merge_error"] = err.Error()
			result.Result["note"] = "changes did not apply to the parent checkout; merge branch " + branch + " manually"
		}
	}
	if merged {
		result.Result["merged"] = true
	}
	// Remove the worktree; keep the branch only when the merge failed.
	_ = exec.CommandContext(ctx, "git", "-C", parentWorkspace, "worktree", "remove", "-f", worktreePath).Run()
	if merged {
		_ = exec.CommandContext(ctx, "git", "-C", parentWorkspace, "branch", "-D", branch).Run()
	}
	return result
}

package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// workspaceBaseline records the workspace's uncommitted state when a run
// starts, so the run's own changes can be told apart from work that was
// already in the checkout: a user's untracked notes, or edits in progress.
// Review and the verification gate look only at what changed since.
//
// The baseline lives for one run. A run interrupted and resumed with a new
// message starts a new baseline, so changes from the interrupted run are not
// reviewed then; they were never claimed complete.
type workspaceBaseline struct {
	git bool
	// dirty maps each uncommitted path to a hash of its content.
	dirty map[string]dirtyFile
}

type dirtyFile struct {
	hash      string
	untracked bool
}

const deletedHash = "<deleted>"

// snapshotWorkspace records the uncommitted files of a git workspace. Outside
// git there is nothing to compare against, and the result says so.
func snapshotWorkspace(ctx context.Context, root string) workspaceBaseline {
	if !isGitWorkspace(ctx, root) {
		return workspaceBaseline{}
	}
	dirty, err := dirtyFiles(ctx, root)
	if err != nil {
		return workspaceBaseline{}
	}
	return workspaceBaseline{git: true, dirty: dirty}
}

// dirtyFiles lists uncommitted paths, untracked files included, with content
// hashes. Orrery's own .orrery directory is not workspace content.
func dirtyFiles(ctx context.Context, root string) (map[string]dirtyFile, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil {
		return nil, err
	}
	files := map[string]dirtyFile{}
	entries := bytes.Split(out, []byte{0})
	for i := 0; i < len(entries); i++ {
		entry := string(entries[i])
		if len(entry) < 4 {
			continue
		}
		status, rel := entry[:2], entry[3:]
		if status[0] == 'R' || status[0] == 'C' {
			// Renames and copies are followed by their source path.
			i++
		}
		if rel == ".orrery" || strings.HasPrefix(rel, ".orrery/") {
			continue
		}
		files[rel] = dirtyFile{hash: hashFile(filepath.Join(root, rel)), untracked: status == "??"}
	}
	return files, nil
}

func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return deletedHash
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return deletedHash
	}
	return hex.EncodeToString(h.Sum(nil))
}

// changes lists the paths this run changed: files dirty now whose content
// differs from the baseline, in sorted order. ok is false when the workspace
// is not a git checkout, and callers fall back to the whole workspace.
func (b workspaceBaseline) changes(ctx context.Context, root string) (paths []string, current map[string]dirtyFile, ok bool) {
	if !b.git {
		return nil, nil, false
	}
	now, err := dirtyFiles(ctx, root)
	if err != nil {
		return nil, nil, false
	}
	for rel, f := range now {
		if before, seen := b.dirty[rel]; !seen || before.hash != f.hash {
			paths = append(paths, rel)
		}
	}
	slices.Sort(paths)
	return paths, now, true
}

// collectChangedDiff renders the run's changes as a unified diff: tracked
// files against HEAD (or the index in a repository without commits), and
// untracked files as new files.
func collectChangedDiff(ctx context.Context, root string, paths []string, current map[string]dirtyFile) ([]byte, error) {
	var tracked, untracked []string
	for _, p := range paths {
		if current[p].untracked {
			untracked = append(untracked, p)
		} else {
			tracked = append(tracked, p)
		}
	}
	var diff []byte
	if len(tracked) > 0 {
		base := []string{"-C", root, "diff", "--no-ext-diff", "--unified=40"}
		if exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--verify", "-q", "HEAD").Run() == nil {
			base = append(base, "HEAD")
		}
		out, err := exec.CommandContext(ctx, "git", append(append(base, "--"), tracked...)...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("tracked diff: %w: %s", err, out)
		}
		diff = out
	}
	for _, rel := range untracked {
		var err error
		if diff, err = appendReviewFile(diff, root, rel); err != nil {
			return nil, err
		}
		if len(diff) >= maxReviewDiff {
			return diff[:maxReviewDiff], nil
		}
	}
	return diff, nil
}

func (e *Engine) setBaseline(sid string, b workspaceBaseline) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.baselines == nil {
		e.baselines = map[string]workspaceBaseline{}
	}
	e.baselines[sid] = b
}

func (e *Engine) clearBaseline(sid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.baselines, sid)
}

func (e *Engine) baseline(sid string) (workspaceBaseline, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.baselines[sid]
	return b, ok
}

// runChanges returns the files this run changed, when that is knowable.
func (e *Engine) runChanges(ctx context.Context, sid, root string) ([]string, map[string]dirtyFile, bool) {
	b, ok := e.baseline(sid)
	if !ok {
		return nil, nil, false
	}
	return b.changes(ctx, root)
}

// reviewDiff returns the diff an independent review should cover: this run's
// changes when they are knowable, otherwise the whole workspace as before.
func (e *Engine) reviewDiff(ctx context.Context, sid, root string) ([]byte, []string, error) {
	paths, current, ok := e.runChanges(ctx, sid, root)
	if !ok {
		diff, err := collectWorkspaceDiff(ctx, root)
		return diff, nil, err
	}
	if len(paths) == 0 {
		return nil, nil, nil
	}
	diff, err := collectChangedDiff(ctx, root, paths, current)
	return diff, paths, err
}

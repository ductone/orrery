package core

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
)

// changedPaths is what this run changed: the workspace delta when knowable,
// plus every file edited through the edit tool.
func (e *Engine) changedPaths(ctx context.Context, sid, root string, progress *progressTracker) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if rel, err := filepath.Rel(root, p); err == nil && filepath.IsAbs(p) && !strings.HasPrefix(rel, "..") {
			p = rel
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if paths, _, ok := e.runChanges(ctx, sid, root); ok {
		for _, p := range paths {
			add(p)
		}
	}
	for p := range progress.editedPaths {
		add(p)
	}
	slices.Sort(out)
	return out
}

// syncWorkspaceChanges makes evidence and review follow the workspace,
// including changes made through exec rather than the edit tool.
func (e *Engine) syncWorkspaceChanges(ctx context.Context, sid, root string, progress *progressTracker) {
	paths, current, ok := e.runChanges(ctx, sid, root)
	if !ok {
		return
	}
	changed := make(map[string]string, len(paths))
	for _, path := range paths {
		changed[path] = current[path].hash
	}
	hash := fingerprint("workspace", changed)
	if hash == progress.workspaceHash {
		return
	}
	previous := progress.workspaceHash
	progress.workspaceHash = hash
	if len(paths) == 0 && previous == "" {
		return
	}
	progress.edited = true
	progress.turnEdited = true
	progress.turnProgress = true
	progress.verified = false
	progress.reviewed = false
	progress.checksSinceEdit = nil
	if progress.editedPaths == nil {
		progress.editedPaths = map[string]bool{}
	}
	for _, path := range paths {
		progress.editedPaths[path] = true
	}
}

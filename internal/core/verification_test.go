package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	return dir, git
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineSeparatesRunChangesFromExistingDirt(t *testing.T) {
	ctx := context.Background()
	dir, git := gitRepo(t)
	writeFile(t, dir, "tracked.go", "package x\n")
	writeFile(t, dir, "userwip.go", "package x\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	// Pre-existing work: an untracked note, an edit in progress, a staged file.
	writeFile(t, dir, "NOTES.md", "private notes\n")
	writeFile(t, dir, "userwip.go", "package x\n// user wip\n")
	writeFile(t, dir, "staged.go", "package x\n")
	git("add", "staged.go")
	b := snapshotWorkspace(ctx, dir)
	if !b.git || len(b.dirty) != 3 {
		t.Fatalf("baseline = %+v", b)
	}
	if paths, _, _ := b.changes(ctx, dir); len(paths) != 0 {
		t.Fatalf("nothing changed yet: %v", paths)
	}
	// The run's changes.
	writeFile(t, dir, "tracked.go", "package x\n// run edit\n")
	writeFile(t, dir, "new/feature.go", "package x\n")
	writeFile(t, dir, "userwip.go", "package x\n// user wip\n// run touched this too\n")
	writeFile(t, dir, ".orrery/jobs/x/result.json", "{}")
	paths, current, ok := b.changes(ctx, dir)
	if !ok || !slices.Equal(paths, []string{"new/feature.go", "tracked.go", "userwip.go"}) {
		t.Fatalf("changes = %v", paths)
	}
	diff, err := collectChangedDiff(ctx, dir, paths, current)
	if err != nil {
		t.Fatal(err)
	}
	text := string(diff)
	for _, want := range []string{"diff --git a/tracked.go", "+// run edit", "diff --git a/new/feature.go", "diff --git a/userwip.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("diff missing %q", want)
		}
	}
	for _, unwanted := range []string{"NOTES.md", "private notes", "staged.go", ".orrery"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("diff must not include %q", unwanted)
		}
	}
}

func TestBaselineWorksWithoutCommitsAndOutsideGit(t *testing.T) {
	ctx := context.Background()
	dir, git := gitRepo(t)
	writeFile(t, dir, "old.go", "package x\n")
	git("add", "old.go")
	b := snapshotWorkspace(ctx, dir)
	writeFile(t, dir, "old.go", "package x\n// changed\n")
	paths, current, ok := b.changes(ctx, dir)
	if !ok || !slices.Equal(paths, []string{"old.go"}) {
		t.Fatalf("changes = %v", paths)
	}
	if diff, err := collectChangedDiff(ctx, dir, paths, current); err != nil || !strings.Contains(string(diff), "+// changed") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	if b := snapshotWorkspace(ctx, t.TempDir()); b.git {
		t.Fatal("a directory outside git has no baseline")
	}
	if _, _, ok := (workspaceBaseline{}).changes(ctx, t.TempDir()); ok {
		t.Fatal("without a baseline, changes are unknowable")
	}
}

func TestReviewCoversOnlyTheRunsChanges(t *testing.T) {
	h := newReviewHarness(t, false)
	h.write("PRIVATE_NOTES.md", "sensitive\n")
	h.write("scratch-todo.md", "people\n")
	h.e.setBaseline(h.sid, snapshotWorkspace(context.Background(), h.workspace))
	h.write("internal/feature.go", "package internal\n")
	h.reviewer = func(string, int) map[string]any { return verdictJSON(true) }
	if passed, _, err := h.run(); err != nil || !passed {
		t.Fatalf("passed=%v err=%v", passed, err)
	}
	spec := h.specs[0]
	if !strings.Contains(spec, "internal/feature.go") || strings.Contains(spec, "PRIVATE_NOTES") || strings.Contains(spec, "scratch-todo") {
		t.Fatalf("review must cover only this run's changes:\n%s", spec)
	}
	if scope := h.events("review.scope"); len(scope) != 1 {
		t.Fatalf("scope events = %v", scope)
	}
}

func TestReviewWithNoRunChangesPasses(t *testing.T) {
	h := newReviewHarness(t, false)
	h.write("NOTES.md", "pre-existing\n")
	h.e.setBaseline(h.sid, snapshotWorkspace(context.Background(), h.workspace))
	h.reviewer = func(string, int) map[string]any { t.Fatal("no reviewer should run"); return nil }
	if passed, text, err := h.run(); err != nil || !passed || text != "no diff" {
		t.Fatalf("passed=%v text=%s err=%v", passed, text, err)
	}
}


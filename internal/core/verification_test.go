package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
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

func TestCompactionGate(t *testing.T) {
	// Each case runs on a fresh gate at turn 10 with 60K tokens in a 100K
	// window, past the floor, so only the boundary itself decides.
	cases := []struct {
		name string
		from string
		to   string
		due  bool
	}{
		{"explore to plan", "explore", "plan", true},
		{"explore to implement", "explore", "implement", true},
		{"wrap-up to implement", "wrap-up", "implement", true},
		{"wrap-up to review", "wrap-up", "review", false},
		{"plan to implement", "plan", "implement", false},
		{"implement to diagnose", "implement", "diagnose", false},
		{"diagnose to implement", "diagnose", "implement", false},
		{"implement to review", "implement", "review", false},
		{"review to implement", "review", "implement", false},
		{"review to diagnose", "review", "diagnose", false},
		{"explore to wrap-up", "explore", "wrap-up", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var g compactionGate
			due, why := g.phaseChange(c.from, c.to, 10, 60_000, 100_000)
			if due != c.due {
				t.Fatalf("%s to %s: due=%v why=%q", c.from, c.to, due, why)
			}
		})
	}

	var g compactionGate
	if due, why := g.phaseChange("explore", "plan", 10, 39_999, 100_000); due || !strings.Contains(why, "floor") {
		t.Fatalf("below the floor: %v %q", due, why)
	}
	if due, _ := g.phaseChange("explore", "implement", 10, 40_000, 100_000); !due {
		t.Fatal("the context floor is enough history to summarise")
	}
	g.record(10)
	if due, why := g.phaseChange("wrap-up", "implement", 12, 60_000, 100_000); due || !strings.Contains(why, "recently") {
		t.Fatalf("too soon: %v %q", due, why)
	}
	// Returning to a phase left this recently is a flip, not a boundary.
	if due, why := g.phaseChange("wrap-up", "explore", 17, 60_000, 100_000); due || !strings.Contains(why, "returned") {
		t.Fatalf("oscillation: %v %q", due, why)
	}
	if due, _ := g.phaseChange("wrap-up", "diagnose", 30, 60_000, 100_000); !due {
		t.Fatal("leaving wrap-up long after the last compaction compacts")
	}
}

// gateRun drives a root session that edits one file and then keeps trying to
// finish, optionally running one command in between.
func gateRun(t *testing.T, file, command string) (agentproto.TaskResult, *scriptedResponses, *Engine, string) {
	t.Helper()
	e, st := testEngine(t)
	workspace, git := gitRepo(t)
	writeFile(t, workspace, "check.sh", "#!/bin/sh\necho checked\n")
	_ = os.Chmod(filepath.Join(workspace, "check.sh"), 0755)
	git("add", "-A")
	git("commit", "-qm", "base")
	writeFile(t, workspace, "docs/rfcs/README.md", "RFCs\n")
	git("add", "-A")
	git("commit", "-qm", "rfcs")
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		if strings.Contains(body["instructions"].(string), "Review this proposed workspace diff") {
			return verdictJSON(true)
		}
		switch {
		case n == 1:
			return responsesCall("e1", "edit", map[string]any{"path": file, "hunks": []any{map[string]any{"anchor": "e3b0c442", "delete": 0, "insert": []any{"content"}}}})
		case n == 2 && command != "":
			return responsesCall("x1", "exec", map[string]any{"command": command})
		}
		return responsesText("Done: wrote " + file)
	}}
	srv := s.serve(t)
	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{Spec: "Write it", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	id, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		_ = st
		return r, s, e, id
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	return agentproto.TaskResult{}, nil, nil, ""
}

func eventTypes(t *testing.T, e *Engine, sid string) []string {
	t.Helper()
	es, err := e.store.EventsAfter(context.Background(), sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range es {
		out = append(out, ev.Type)
	}
	return out
}

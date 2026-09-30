package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestIsVerificationCommand(t *testing.T) {
	for _, cmd := range []string{
		"go test ./...", "go vet ./internal/...", "cd /repo && go build ./cmd/x",
		"timeout 120 go test -run TestX ./pkg", "env CI=1 npm test", "npm run lint -- --fix=false",
		"pnpm typecheck", "pnpm run build", "yarn lint", "make test", "make lint/md", "make check-all",
		"npx eslint src/", "npx --yes eslint .", "pnpm exec tsc --noEmit", "yarn dlx prettier --check .",
		"bunx vitest run", "python -m pytest -q", "python3 -m mypy app", "uv run ruff check .",
		"poetry run pytest", "bundle exec rspec spec/models", "./gradlew test", "mvn -q verify",
		"cargo clippy -- -D warnings", "cargo fmt --check", "golangci-lint run ./...", "gofmt -l .",
		"shellcheck scripts/*.sh", "npx markdownlint-cli2 --no-globs docs/rfc.md", "markdownlint docs",
		"npx prettier --check docs/rfc.md", "prettier -c .", "buf lint", "terraform validate", "tsc -p .",
		"go test ./... 2>&1 | tail -30", "git diff --check && go test ./...", "ls; make build",
		"go tool staticcheck ./...", "sudo -u ci make ci",
	} {
		if !isVerificationCommand(cmd) {
			t.Errorf("not recognised: %q", cmd)
		}
	}
	for _, cmd := range []string{
		"cat tsconfig.json", "cat jest.config.js", "ls tox.ini", "grep -rn eslint package.json",
		"echo tsc", "echo 'go test'", "git diff --check", "git status", "sed -n 1,20p Makefile",
		"prettier --write .", "npx prettier docs/rfc.md", "find . -name '*_test.go'", "go mod tidy",
		"go run ./cmd/tool", "make", "make install", "npm install", "pnpm run dev", "ruffle", "gotest",
		"rg 'make test' docs", "cargo fmt", "", "echo \"unterminated",
	} {
		if isVerificationCommand(cmd) {
			t.Errorf("recognised as verification: %q", cmd)
		}
	}
}

func TestNeedsVerification(t *testing.T) {
	for _, tc := range []struct {
		paths []string
		want  bool
	}{
		{[]string{"docs/rfcs/proposal.md"}, false},
		{[]string{"docs/a.md", "docs/b.rst", "img/logo.svg", "notes.txt"}, false},
		{[]string{"docs/a.md", "Makefile"}, true},
		{[]string{"ci/pipeline.yaml"}, true},
		{[]string{"config.json"}, true},
		{[]string{"main.go"}, true},
		{[]string{"go.sum"}, true},
		{nil, true},
	} {
		if got := needsVerification(tc.paths); got != tc.want {
			t.Errorf("needsVerification(%v) = %v, want %v", tc.paths, got, tc.want)
		}
	}
}

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
	var g compactionGate
	if due, why := g.phaseChange("plan", "implement", 3, 10_000); due || !strings.Contains(why, "too small") {
		t.Fatalf("small history: %v %q", due, why)
	}
	if due, _ := g.phaseChange("implement", "review", 10, 50_000); !due {
		t.Fatal("a real boundary with history compacts")
	}
	g.record(10)
	if due, why := g.phaseChange("review", "wrap-up", 12, 50_000); due || !strings.Contains(why, "recently") {
		t.Fatalf("too soon: %v %q", due, why)
	}
	// Flipping back to a phase just left is not a boundary.
	if due, why := g.phaseChange("wrap-up", "review", 17, 50_000); due || !strings.Contains(why, "returned") {
		t.Fatalf("oscillation: %v %q", due, why)
	}
	if due, _ := g.phaseChange("review", "diagnose", 30, 50_000); !due {
		t.Fatal("a new phase long after the last compaction compacts")
	}
}

// gateRun drives a root session that edits one file and then keeps trying to
// finish, optionally running one command in between.
func gateRun(t *testing.T, file, command string, jevScore float64) (agentproto.TaskResult, *scriptedResponses, *Engine, string) {
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
		Interventions: config.InterventionConfig{JudgeEnabled: new(bool)},
	}
	if jevScore >= 0 {
		jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"meaningful_check": map[string]any{"type": "noul", "noul": jevScore}}})
		}))
		t.Cleanup(jevSrv.Close)
		cfg.Jev = config.JevConfig{APIKey: "k", BaseURL: jevSrv.URL, Review: true}
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

func TestProseOnlyChangesNeedNoVerification(t *testing.T) {
	result, _, e, sid := gateRun(t, "docs/rfcs/proposal.md", "", -1)
	if result.Status != agentproto.Pass || result.Outcome.CompletionRejects != 0 {
		t.Fatalf("result = %+v", result)
	}
	if !slices.Contains(eventTypes(t, e, sid), "verification.accepted") {
		t.Fatal("the skipped gate must be recorded")
	}
}

func TestUnverifiedCodeIsRejectedThenWaived(t *testing.T) {
	result, s, e, sid := gateRun(t, "main.go", "", -1)
	if result.Status != agentproto.Pass || result.Outcome.Verified {
		t.Fatalf("an unverifiable change completes, recorded as unverified: %+v", result)
	}
	if result.Outcome.CompletionRejects != maxVerificationRejections {
		t.Fatalf("rejections = %d, want %d", result.Outcome.CompletionRejects, maxVerificationRejections)
	}
	if !slices.Contains(eventTypes(t, e, sid), "verification.waived") {
		t.Fatal("the waiver must be recorded")
	}
	var nudge string
	for _, req := range s.requests {
		for _, raw := range req["input"].([]any) {
			if m, _ := raw.(map[string]any); m != nil {
				if c, _ := m["content"].(string); strings.Contains(c, "no successful command checked them") {
					nudge = c
				}
			}
		}
	}
	if !strings.Contains(nudge, "say so in your final result") || !strings.Contains(nudge, "Do not add or change build targets") {
		t.Fatalf("rejection message = %q", nudge)
	}
	if !strings.Contains(s.requests[0]["instructions"].(string), "only to satisfy a harness check") {
		t.Fatal("the system prompt must forbid manufacturing checks")
	}
}

func TestJevAcceptsAnUnrecognisedMeaningfulCheck(t *testing.T) {
	result, _, e, sid := gateRun(t, "main.go", "./check.sh main.go", 0.9)
	if result.Status != agentproto.Pass || result.Outcome.CompletionRejects != 0 || !result.Outcome.Verified {
		t.Fatalf("result = %+v", result)
	}
	if !slices.Contains(eventTypes(t, e, sid), "verification.judged") {
		t.Fatal("the classifier's judgement must be recorded")
	}
}

func TestJevRejectsACommandThatCheckedNothing(t *testing.T) {
	result, _, _, _ := gateRun(t, "main.go", "./check.sh main.go", 0.1)
	if result.Outcome.CompletionRejects == 0 || result.Outcome.Verified {
		t.Fatalf("result = %+v", result)
	}
}

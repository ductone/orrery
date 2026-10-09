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

func TestProseOnlyChangesNeedNoVerification(t *testing.T) {
	result, _, e, sid := gateRun(t, "docs/rfcs/proposal.md", "")
	if result.Status != agentproto.Pass || result.Outcome.CompletionRejects != 0 {
		t.Fatalf("result = %+v", result)
	}
	events := eventTypes(t, e, sid)
	if !slices.Contains(events, "verification.accepted") {
		t.Fatal("the acceptance must be recorded")
	}
	if slices.Contains(events, "verification.advised") {
		t.Fatal("a prose-only change must not be advised to verify")
	}
}

func TestUnverifiedCodeIsAdvisedOnceWithoutRejecting(t *testing.T) {
	result, s, e, sid := gateRun(t, "main.go", "")
	if result.Status != agentproto.Pass || result.Outcome.Verified {
		t.Fatalf("an unverifiable change completes, recorded as unverified: %+v", result)
	}
	if result.Outcome.CompletionRejects != 0 {
		t.Fatalf("verification must not reject completion: %+v", result.Outcome)
	}
	notes := 0
	for _, typ := range eventTypes(t, e, sid) {
		if typ == "verification.advised" {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("verification notes = %d, want one per set of changes", notes)
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
		t.Fatalf("advisory message = %q", nudge)
	}
	if !strings.Contains(s.requests[0]["instructions"].(string), "only to satisfy a harness check") {
		t.Fatal("the system prompt must forbid manufacturing checks")
	}
}

func TestUnrecognisedCheckOnlyAdvises(t *testing.T) {
	result, _, e, sid := gateRun(t, "main.go", "./check.sh main.go")
	if result.Status != agentproto.Pass || result.Outcome.Verified {
		t.Fatalf("recognition is mechanical, so an unknown command cannot verify: %+v", result)
	}
	if result.Outcome.CompletionRejects != 0 {
		t.Fatalf("an unrecognised check must not reject completion: %+v", result.Outcome)
	}
	if slices.Contains(eventTypes(t, e, sid), "verification.judged") {
		t.Fatal("the Jev verification judge is removed")
	}
}

func TestVerificationKinds(t *testing.T) {
	for cmd, want := range map[string]string{
		"cargo fmt --manifest-path src-tauri/Cargo.toml -- --check": formatCheck,
		"npx prettier --check docs/rfc.md":                          formatCheck,
		"gofmt -l .":                                                formatCheck,
		"npx markdownlint-cli2 --no-globs docs/rfc.md":              formatCheck,
		"yamllint ci.yaml":                                          formatCheck,
		"cargo check --manifest-path src-tauri/Cargo.toml":          fullCheck,
		"gofmt -l . && go test ./...":                               fullCheck,
		"go test ./... && gofmt -l .":                               fullCheck,
		"pnpm typecheck":                                            fullCheck,
		"cat Cargo.toml":                                            notACheck,
	} {
		if got := verificationKind(cmd); got != want {
			t.Errorf("verificationKind(%q) = %q, want %q", cmd, got, want)
		}
	}
	if changesCode([]string{"ci/pipeline.yaml", "docs/a.md"}) || !changesCode([]string{"ci/pipeline.yaml", "src/lib.rs"}) {
		t.Fatal("changesCode misclassified")
	}
}

func TestFormatCheckDoesNotVerifyCode(t *testing.T) {
	result, _, _, _ := gateRun(t, "main.go", "gofmt -l .")
	if result.Outcome.Verified || result.Outcome.CompletionRejects != 0 {
		t.Fatalf("a format check must not verify a code change: %+v", result.Outcome)
	}
}

func TestFormatCheckVerifiesConfiguration(t *testing.T) {
	result, _, e, sid := gateRun(t, "pipeline.yaml", "gofmt -l .")
	if result.Outcome.CompletionRejects != 0 {
		t.Fatalf("a format check covers a configuration change: %+v", result.Outcome)
	}
	if !slices.Contains(eventTypes(t, e, sid), "verification.accepted") {
		t.Fatal("the acceptance must be recorded")
	}
}

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ductone/orrey/internal/hashline"
)

func TestExecFailureIsAnError(t *testing.T) {
	r := New(t.TempDir())
	v, err := r.Call(context.Background(), "exec", map[string]any{"command": "echo marker; exit 7"})
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("value=%v error=%v", v, err)
	}
}
func TestExecRejectsSourceMutationFallbacks(t *testing.T) {
	r := New(t.TempDir())
	for _, command := range []string{
		`touch pkg/new.go`,
		`python3 -c 'from pathlib import Path; Path("pkg/new.go").write_text("package x")'`,
		`gofmt -w pkg/file.go`,
		`printf 'package x' > pkg/new.go`,
	} {
		if _, err := r.Call(context.Background(), "exec", map[string]any{"command": command}); err == nil || !strings.Contains(err.Error(), "edit tool") {
			t.Fatalf("command %q error=%v", command, err)
		}
	}
	if _, err := r.Call(context.Background(), "exec", map[string]any{"command": `true 2>/dev/null`}); err != nil {
		t.Fatalf("read-only command rejected: %v", err)
	}
}
func TestReadOnlyRegistryOmitsMutationAndShellTools(t *testing.T) {
	r := NewReadOnly(t.TempDir())
	names := map[string]bool{}
	for _, d := range r.Definitions() {
		names[d.Name] = true
	}
	if !names["read"] || !names["search"] || names["edit"] || names["exec"] || names["job"] {
		t.Fatalf("tools=%v", names)
	}
}
func TestDefinitionsExcept(t *testing.T) {
	r := New(t.TempDir())
	for _, d := range r.DefinitionsExcept("read", "search") {
		if d.Name == "read" || d.Name == "search" {
			t.Fatalf("definition %q not excluded", d.Name)
		}
	}
}
func TestDefinitionsOnly(t *testing.T) {
	r := New(t.TempDir())
	definitions := r.DefinitionsOnly("edit")
	if len(definitions) != 1 || definitions[0].Name != "edit" {
		t.Fatalf("definitions=%v", definitions)
	}
}

func TestEditStaleAnchorReturnsFreshAnchorsAndRecoveryHint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.go")
	if err := os.WriteFile(path, []byte("package sample\n\nfunc run() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(root)
	if _, err := r.Call(context.Background(), "read", map[string]any{"path": "file.go"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Call(context.Background(), "edit", map[string]any{
		"path":  "file.go",
		"hunks": []any{map[string]any{"anchor": "deadbeef", "delete": float64(1), "insert": []any{"package changed"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "fresh anchors") || !strings.Contains(err.Error(), "call read") || !strings.Contains(err.Error(), `"hash"`) {
		t.Fatalf("error=%v", err)
	}
}

func TestAttachmentSchemeAllowsOnlyExactRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.txt")
	if err := os.WriteFile(path, []byte("bounded evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewReadOnly(t.TempDir())
	r.AddFileScheme("attachment", map[string]string{"a-1": path})
	value, err := r.Call(context.Background(), "read", map[string]any{"path": "attachment://a-1"})
	if err != nil {
		t.Fatal(err)
	}
	if value.(map[string]any)["text"] != "bounded evidence" {
		t.Fatalf("value = %#v", value)
	}
	if _, err := r.Call(context.Background(), "read", map[string]any{"path": "attachment://../evidence.txt"}); err == nil {
		t.Fatal("attachment scheme accepted a non-allowlisted path")
	}
}

func TestSearchGlobMatchesWorkspaceRelativePathRecursively(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "frontend", "components", "core", "view.tsx")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("const marker = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(root)
	v, err := r.Call(context.Background(), "search", map[string]any{"pattern": "marker", "glob": "frontend/**/*.tsx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.([]map[string]any)) != 1 {
		t.Fatalf("results=%v", v)
	}
}

func TestGlobMatchSupportsBraceAlternatives(t *testing.T) {
	if !globMatch("frontend/**/*.{tsx,ts}", "frontend/components/core/view.tsx") {
		t.Fatal("brace glob did not match")
	}
	if globMatch("frontend/**/*.{tsx,ts}", "frontend/components/core/view.go") {
		t.Fatal("brace glob matched wrong suffix")
	}
}

func TestSearchPrunesDependencyTrees(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"frontend/view.tsx", "vendor/ignored.tsx", "node_modules/ignored.tsx"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("marker\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := New(root)
	v, err := r.Call(context.Background(), "search", map[string]any{"pattern": "marker"})
	if err != nil {
		t.Fatal(err)
	}
	results := v.([]map[string]any)
	if len(results) != 1 || results[0]["path"] != filepath.Join("frontend", "view.tsx") {
		t.Fatalf("results=%v", results)
	}
}

func TestCommandSummaryMarksOmittedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprint(i)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := commandSummary(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.(map[string]any)["summary"].(string), "lines omitted") {
		t.Fatalf("summary=%v", v)
	}
}
func TestReadScheme(t *testing.T) {
	r := New(t.TempDir())
	r.AddScheme("job", func(context.Context, map[string]any) (any, error) { return "ok", nil })
	v, err := r.Call(context.Background(), "read", map[string]any{"path": "job://123/result.answer"})
	if err != nil || v != "ok" {
		t.Fatalf("%v %v", v, err)
	}
}

func TestReadAroundLineAndLargeFileOutline(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(root)
	v, err := r.Call(context.Background(), "read", map[string]any{"path": "small.txt", "around_line": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	window := v.([]hashline.Line)
	if len(window) != 5 || window[2].Text != "three" || window[2].Hash == "" {
		t.Fatalf("window=%#v", window)
	}
	v, err = r.Call(context.Background(), "read", map[string]any{"path": "small.txt", "around_line": float64(99)})
	if err != nil || v.([]hashline.Line)[len(v.([]hashline.Line))-1].Text != "five" {
		t.Fatalf("clamped window=%#v err=%v", v, err)
	}
	v, err = r.Call(context.Background(), "read", map[string]any{"path": "small.txt", "start": float64(3), "limit": float64(1), "around_line": nil})
	if err != nil || len(v.([]hashline.Line)) != 1 || v.([]hashline.Line)[0].Text != "three" {
		t.Fatalf("null around_line window=%#v err=%v", v, err)
	}
	large := make([]string, 2002)
	large[0], large[1000], large[2001] = "package sample", "func middle() {}", "end"
	if err := os.WriteFile(filepath.Join(root, "large.go"), []byte(strings.Join(large, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	v, err = r.Call(context.Background(), "read", map[string]any{"path": "large.go"})
	if err != nil {
		t.Fatal(err)
	}
	summary := v.(map[string]any)
	if !summary["summarized"].(bool) || len(summary["outline"].([]hashline.Line)) != 2 || !strings.Contains(summary["hint"].(string), "around_line") {
		t.Fatalf("summary=%#v", summary)
	}
	v, err = r.Call(context.Background(), "read", map[string]any{"path": "large.go", "start": float64(1000), "limit": float64(2)})
	if err != nil || len(v.([]hashline.Line)) != 2 {
		t.Fatalf("window=%#v err=%v", v, err)
	}
}

func TestEditFailureLadderAndNoopLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := &SessionState{}
	r := NewWithState(root, state)
	if _, err := r.Call(context.Background(), "read", map[string]any{"path": "file.txt"}); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{"path": "file.txt", "hunks": []any{map[string]any{"anchor": "deadbeef", "delete": float64(1), "insert": []any{"changed"}}}}
	if _, err := r.Call(context.Background(), "edit", stale); err == nil || !strings.Contains(err.Error(), "fresh anchors") {
		t.Fatalf("first stale: %v", err)
	}
	// The engine rebuilds its registry every model turn. Recovery state must
	// survive that boundary.
	r = NewWithState(root, state)
	v, err := r.Call(context.Background(), "edit", stale)
	if err == nil || !strings.Contains(err.Error(), "fresh unique anchor") {
		t.Fatalf("second stale: %v", err)
	}
	ladder := v.(map[string]any)
	if _, ok := ladder["occurrence_lines"]; !ok || ladder["directive"] == "" {
		t.Fatalf("ladder=%#v", ladder)
	}
	read, err := r.Call(context.Background(), "read", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	anchor := read.([]hashline.Line)[0].Hash
	noop := map[string]any{"path": "file.txt", "hunks": []any{map[string]any{"anchor": anchor, "delete": float64(1), "insert": []any{"one"}}}}
	for i := 0; i < 2; i++ {
		r = NewWithState(root, state)
		if _, err := r.Call(context.Background(), "edit", noop); !errors.Is(err, hashline.ErrNoChanges) {
			t.Fatalf("noop %d: %v", i, err)
		}
	}
	r = NewWithState(root, state)
	if _, err := r.Call(context.Background(), "edit", noop); err == nil || !strings.Contains(err.Error(), "E_NOOP_LOOP") {
		t.Fatalf("loop error: %v", err)
	}
}

func TestEditRejectsFileChangedSinceRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := &SessionState{}
	r := NewWithStateDialect(root, state, "hashline-contextual")
	value, err := r.Call(context.Background(), "read", map[string]any{"path": "file.txt", "around_line": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	anchor := value.([]hashline.Line)[1].Hash
	if err := os.WriteFile(path, []byte("external\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	details, err := r.Call(context.Background(), "edit", map[string]any{
		"path":  "file.txt",
		"hunks": []any{map[string]any{"anchor": anchor, "delete": float64(1), "insert": []any{"changed"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "E_FILE_CHANGED") {
		t.Fatalf("error=%v details=%#v", err, details)
	}
	payload := details.(map[string]any)
	if payload["expected_version"] == payload["current_version"] || payload["diff_since_read"] == nil {
		t.Fatalf("details=%#v", payload)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "external\ntwo\nthree\n" {
		t.Fatalf("conflicting edit modified the file: %q", b)
	}
}

func TestAroundLineHonorsLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(root)
	value, err := r.Call(context.Background(), "read", map[string]any{"path": "file.txt", "around_line": float64(3), "limit": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	window := value.([]hashline.Line)
	if len(window) != 3 || window[0].Text != "two" || window[2].Text != "four" {
		t.Fatalf("window=%#v", window)
	}
}

func TestConcurrentSessionsDoNotOverwriteSameSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registries := []*Registry{NewWithState(root, &SessionState{}), NewWithState(root, &SessionState{})}
	anchors := make([]string, len(registries))
	for i, registry := range registries {
		value, err := registry.Call(context.Background(), "read", map[string]any{"path": "file.txt"})
		if err != nil {
			t.Fatal(err)
		}
		anchors[i] = value.([]hashline.Line)[1].Hash
	}
	type outcome struct {
		err error
	}
	outcomes := make(chan outcome, len(registries))
	var wg sync.WaitGroup
	for i, registry := range registries {
		wg.Add(1)
		go func(i int, registry *Registry) {
			defer wg.Done()
			_, err := registry.Call(context.Background(), "edit", map[string]any{
				"path":  "file.txt",
				"hunks": []any{map[string]any{"anchor": anchors[i], "delete": float64(1), "insert": []any{fmt.Sprintf("writer-%d", i)}}},
			})
			outcomes <- outcome{err: err}
		}(i, registry)
	}
	wg.Wait()
	close(outcomes)
	succeeded, conflicted := 0, 0
	for result := range outcomes {
		switch {
		case result.err == nil:
			succeeded++
		case strings.Contains(result.err.Error(), "E_FILE_CHANGED"):
			conflicted++
		default:
			t.Fatalf("unexpected edit error: %v", result.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestOptimisticLockDetectsFinalNewlineOnlyChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewWithState(root, &SessionState{})
	value, err := r.Call(context.Background(), "read", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	anchor := value.([]hashline.Line)[0].Hash
	if err := os.WriteFile(path, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = r.Call(context.Background(), "edit", map[string]any{
		"path":  "file.txt",
		"hunks": []any{map[string]any{"anchor": anchor, "delete": float64(1), "insert": []any{"changed"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "E_FILE_CHANGED") {
		t.Fatalf("final-newline-only change was not detected: %v", err)
	}
}

func TestSuccessfulEditRefreshesOptimisticLockSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewWithState(root, &SessionState{})
	value, err := r.Call(context.Background(), "read", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	firstAnchor := value.([]hashline.Line)[0].Hash
	value, err = r.Call(context.Background(), "edit", map[string]any{
		"path":  "file.txt",
		"hunks": []any{map[string]any{"anchor": firstAnchor, "delete": float64(1), "insert": []any{"ONE"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	windows := value.(map[string]any)["fresh_anchors"].([][]hashline.Line)
	var secondAnchor string
	for _, line := range windows[0] {
		if line.Text == "two" {
			secondAnchor = line.Hash
		}
	}
	if secondAnchor == "" {
		t.Fatalf("fresh window omitted second line: %#v", windows)
	}
	_, err = r.Call(context.Background(), "edit", map[string]any{
		"path":  "file.txt",
		"hunks": []any{map[string]any{"anchor": secondAnchor, "delete": float64(1), "insert": []any{"TWO"}}},
	})
	if err != nil {
		t.Fatalf("second edit using returned fresh anchor failed: %v", err)
	}
}

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecFailureIsAnError(t *testing.T) {
	r := New(t.TempDir())
	v, err := r.Call(context.Background(), "exec", map[string]any{"command": "echo marker; exit 7"})
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("value=%v error=%v", v, err)
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

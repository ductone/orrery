package tools

import (
	"context"
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
func TestReadScheme(t *testing.T) {
	r := New(t.TempDir())
	r.AddScheme("job", func(context.Context, map[string]any) (any, error) { return "ok", nil })
	v, err := r.Call(context.Background(), "read", map[string]any{"path": "job://123/result.answer"})
	if err != nil || v != "ok" {
		t.Fatalf("%v %v", v, err)
	}
}

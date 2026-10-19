package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/ductone/orrery/internal/hashline"
	"github.com/ductone/orrery/workspace"
)

// TestVirtualWorkspaceTools runs the file tools over a workspace that exists
// only in memory.
func TestVirtualWorkspaceTools(t *testing.T) {
	ctx := context.Background()
	mem := workspace.NewMemory()
	if err := mem.MkdirAll(ctx, "notes", 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewForWorkspace(workspace.Virtual(mem), nil, string(anchorHashline), false)

	var names []string
	for _, d := range r.Definitions() {
		names = append(names, d.Name)
	}
	got := strings.Join(names, ",")
	if strings.Contains(got, "exec") || strings.Contains(got, "job") || strings.Contains(got, "ast_edit") || !strings.Contains(got, "edit") || !strings.Contains(got, "search") {
		t.Fatalf("tools = %s; want file tools without exec, job or ast_edit", got)
	}

	if _, err := r.Call(ctx, "write", map[string]any{"path": "notes/plan.md", "content": "alpha\nbeta\ngamma"}); err != nil {
		t.Fatal(err)
	}
	read, err := r.Call(ctx, "read", map[string]any{"path": "notes/plan.md"})
	if err != nil {
		t.Fatal(err)
	}
	lines := read.([]hashline.Line)
	if len(lines) != 3 || lines[1].Text != "beta" {
		t.Fatalf("read = %+v", lines)
	}
	if _, err := r.Call(ctx, "edit", map[string]any{"path": "notes/plan.md", "hunks": []any{map[string]any{"anchor": lines[1].Hash[:8], "delete": 1, "insert": []any{"BETA"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Call(ctx, "replace", map[string]any{"old_string": "gamma", "new_string": "GAMMA", "paths": []any{"notes"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := mem.ReadFile(ctx, "notes/plan.md")
	if string(data) != "alpha\nBETA\nGAMMA\n" {
		t.Fatalf("file = %q", data)
	}

	found, err := r.Call(ctx, "search", map[string]any{"pattern": "BETA"})
	if err != nil {
		t.Fatal(err)
	}
	if paths := SearchResultPaths(found); len(paths) != 1 || paths[0] != "notes/plan.md" {
		t.Fatalf("search paths = %v (result %+v)", paths, found)
	}

	if _, err := r.Call(ctx, "read", map[string]any{"path": "../outside"}); err == nil {
		t.Fatal("read escaped the virtual workspace")
	}
	dir, err := r.Call(ctx, "read", map[string]any{"path": "."})
	if err != nil {
		t.Fatal(err)
	}
	if entries := dir.([]map[string]any); len(entries) != 1 || entries[0]["name"] != "notes" {
		t.Fatalf("root listing = %+v", dir)
	}
}

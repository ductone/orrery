package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/hashline"
)

func TestEditReportsAmbiguousAnchorsAndAcceptsLineHints(t *testing.T) {
	root := t.TempDir()
	body := "fn new() {\n    state_dir,\n}\n\nfn with_effects() {\n    state_dir,\n}\n"
	path := filepath.Join(root, "lock.rs")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	r := New(root)
	ctx := context.Background()
	if _, err := r.Call(ctx, "read", map[string]any{"path": "lock.rs"}); err != nil {
		t.Fatal(err)
	}
	lines, _ := hashline.Read(path)
	anchor := lines[5].Hash
	hunk := map[string]any{"anchor": anchor, "delete": float64(1), "insert": []any{"    state_dir: dir,"}}
	v, err := r.Call(ctx, "edit", map[string]any{"path": "lock.rs", "hunks": []any{hunk}})
	if err == nil || !strings.Contains(err.Error(), "ambiguous anchor") || strings.Contains(err.Error(), "stale") {
		t.Fatalf("err = %v", err)
	}
	out := v.(map[string]any)
	if got := out["matching_lines"].([]int); len(got) != 2 || got[0] != 2 || got[1] != 6 {
		t.Fatalf("matching lines = %v", got)
	}
	hunk["line"] = float64(6)
	if _, err := r.Call(ctx, "edit", map[string]any{"path": "lock.rs", "hunks": []any{hunk}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "fn new() {\n    state_dir,\n") || !strings.Contains(string(b), "with_effects() {\n    state_dir: dir,\n") {
		t.Fatalf("content:\n%s", b)
	}
	for _, d := range r.Definitions() {
		if d.Name == "edit" && !strings.Contains(d.Description, "pass its line number") {
			t.Fatal("the edit description must explain line hints")
		}
	}
}

func TestJobToolFallsBackForIDsItDidNotStart(t *testing.T) {
	r := New(t.TempDir())
	if _, err := r.Call(context.Background(), "job", map[string]any{"id": "w1", "action": "wait"}); err == nil {
		t.Fatal("without a fallback an unknown id is not found")
	}
	r.SetJobFallback(func(_ context.Context, id, action string) (any, error) {
		return map[string]any{"id": id, "action": action, "kind": "worker"}, nil
	})
	v, err := r.Call(context.Background(), "job", map[string]any{"id": "w1", "action": "wait"})
	if err != nil || v.(map[string]any)["kind"] != "worker" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

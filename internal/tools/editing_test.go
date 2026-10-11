package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/hashline"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteCreatesOverwritesAndFeedsEdit(t *testing.T) {
	root := t.TempDir()
	r := New(root)
	ctx := context.Background()
	v, err := r.Call(ctx, "write", map[string]any{"path": "pkg/a.go", "content": "package a\n\nvar x = 1"})
	if err == nil {
		t.Fatal("write into a missing directory must fail rather than invent directories silently")
	}
	_ = v
	v, err = r.Call(ctx, "write", map[string]any{"path": "a.go", "content": "package a\n\nvar x = 1"})
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	if out["created"] != true || readFile(t, filepath.Join(root, "a.go")) != "package a\n\nvar x = 1\n" {
		t.Fatalf("create: %v", out)
	}
	v, err = r.Call(ctx, "write", map[string]any{"path": "a.go", "content": "package a\n\nvar y = 2\n"})
	if err != nil {
		t.Fatal(err)
	}
	out = v.(map[string]any)
	if out["created"] != false || readFile(t, filepath.Join(root, "a.go")) != "package a\n\nvar y = 2\n" {
		t.Fatalf("overwrite: %v", out)
	}
	anchors := out["anchors"].([]hashline.Line)
	hunk := map[string]any{"anchor": anchors[2].Hash, "delete": float64(1), "insert": []any{"var y = 3"}}
	if _, err := r.Call(ctx, "edit", map[string]any{"path": "a.go", "hunks": []any{hunk}}); err != nil {
		t.Fatalf("edit with write anchors: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "a.go")); got != "package a\n\nvar y = 3\n" {
		t.Fatalf("after edit = %q", got)
	}
	if _, err := r.Call(ctx, "write", map[string]any{"path": "../outside.go", "content": "x"}); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("outside write err = %v", err)
	}
}

func TestWriteAppendDoesNotInsertBlankLines(t *testing.T) {
	root := t.TempDir()
	r := New(root)
	ctx := context.Background()
	for _, chunk := range []string{"a", "b\n", "c"} {
		if _, err := r.Call(ctx, "write", map[string]any{"path": "log.txt", "content": chunk, "append": true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readFile(t, filepath.Join(root, "log.txt")); got != "a\nb\nc\n" {
		t.Fatalf("appended = %q", got)
	}
}

func TestReplaceRenamesAcrossFilesAndFeedsEdit(t *testing.T) {
	root := t.TempDir()
	for i, name := range []string{"a.go", "b.go", "sub/c.go"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755)
		body := "package p\n\nfunc use" + string(rune('A'+i)) + "() { OldName() }\n"
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := New(root)
	ctx := context.Background()
	v, err := r.Call(ctx, "replace", map[string]any{"old_string": "OldName", "new_string": "NewName", "paths": []any{"a.go", "b.go", "sub"}})
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	if out["total"] != 3 || len(out["replaced"].([]map[string]any)) != 3 {
		t.Fatalf("replace result = %v", out)
	}
	first := out["replaced"].([]map[string]any)[0]
	changed := first["changed_lines"].([]map[string]any)
	if len(changed) != 1 || changed[0]["line"] != 3 {
		t.Fatalf("changed lines = %v", changed)
	}
	for _, name := range []string{"a.go", "b.go", "sub/c.go"} {
		if got := readFile(t, filepath.Join(root, name)); strings.Contains(got, "OldName") || !strings.Contains(got, "NewName") {
			t.Fatalf("%s = %q", name, got)
		}
	}
	hunk := map[string]any{"anchor": changed[0]["hash"], "delete": float64(1), "insert": []any{"func useA() { NewName(); NewName() }"}}
	if _, err := r.Call(ctx, "edit", map[string]any{"path": "a.go", "hunks": []any{hunk}}); err != nil {
		t.Fatalf("edit after replace: %v", err)
	}
}

func TestReplaceAmbiguousFailsWithLinesAndLeavesFilesUntouched(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("foo\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("foo\nbar\nfoo\n"), 0o644)
	r := New(root)
	ctx := context.Background()
	_, err := r.Call(ctx, "replace", map[string]any{"old_string": "foo", "new_string": "baz", "paths": []any{"a.txt", "b.txt"}})
	if err == nil || !strings.Contains(err.Error(), "lines [1 3]") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, filepath.Join(root, "a.txt")) != "foo\n" {
		t.Fatal("a failed replace must not modify other files")
	}
	if _, err := r.Call(ctx, "replace", map[string]any{"old_string": "foo", "new_string": "baz", "paths": []any{"b.txt"}, "replace_all": true}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "b.txt")); got != "baz\nbar\nbaz\n" {
		t.Fatalf("replace_all = %q", got)
	}
	if _, err := r.Call(ctx, "replace", map[string]any{"old_string": "x", "new_string": "y", "paths": []any{"../elsewhere"}}); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("outside replace err = %v", err)
	}
}

func TestChangedLineEntriesTrimsUnchangedTail(t *testing.T) {
	before := []string{"a", "b", "c", ""}
	after := []string{"a", "b1", "b2", "c", ""}
	fresh := []hashline.Line{{Number: 1, Hash: "h1"}, {Number: 2, Hash: "h2"}, {Number: 3, Hash: "h3"}, {Number: 4, Hash: "h4"}}
	got := changedLineEntries(before, after, fresh)
	if len(got) != 2 || got[0]["line"] != 2 || got[1]["line"] != 3 {
		t.Fatalf("changed = %v", got)
	}
}

func TestEditAfterExecChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644)
	r := New(root)
	ctx := context.Background()
	if _, err := r.Call(ctx, "read", map[string]any{"path": "f.txt"}); err != nil {
		t.Fatal(err)
	}
	lines, _ := hashline.Read(path)
	if _, err := r.Call(ctx, "exec", map[string]any{"command": "printf 'one\\nTWO\\nthree\\nfour\\n' > f.txt"}); err != nil {
		t.Fatal(err)
	}
	// The anchor for "two" is gone: the edit names the command and the lines.
	stale := map[string]any{"anchor": lines[1].Hash, "delete": float64(1), "insert": []any{"2"}}
	_, err := r.Call(ctx, "edit", map[string]any{"path": "f.txt", "hunks": []any{stale}})
	if err == nil || !strings.Contains(err.Error(), "changed by your last command; re-read lines 2-2") {
		t.Fatalf("stale edit err = %v", err)
	}
	// The anchor for "four" still resolves uniquely, so the edit applies.
	live := map[string]any{"anchor": lines[3].Hash, "delete": float64(1), "insert": []any{"4"}}
	v, err := r.Call(ctx, "edit", map[string]any{"path": "f.txt", "hunks": []any{live}})
	if err != nil {
		t.Fatalf("unique anchor after exec: %v", err)
	}
	if note, _ := v.(map[string]any)["note"].(string); !strings.Contains(note, "last command") {
		t.Fatalf("note = %q", note)
	}
	if got := readFile(t, path); got != "one\nTWO\nthree\n4\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestAstEditRegistrationFollowsPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, d := range New(t.TempDir()).Definitions() {
		if d.Name == "ast_edit" {
			t.Fatal("ast_edit must be absent when ast-grep is not on PATH")
		}
	}
}

func TestAstEditRewritesGoAndPython(t *testing.T) {
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not installed")
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc f() int { return CalcTotal(3,4) }\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package a\n\nvar v = CalcTotal(1, 2)\n"), 0o644)
	os.WriteFile(filepath.Join(root, "c.py"), []byte("x = old_fn(1)\n"), 0o644)
	r := New(root)
	ctx := context.Background()
	v, err := r.Call(ctx, "ast_edit", map[string]any{"ops": []any{map[string]any{"pattern": "CalcTotal($A, $B)", "rewrite": "TotalCents($A, $B)"}}, "paths": []any{"*.go"}, "lang": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if v.(map[string]any)["changed_files"] != 2 {
		t.Fatalf("result = %v", v)
	}
	if got := readFile(t, filepath.Join(root, "a.go")); !strings.Contains(got, "TotalCents(3, 4)") {
		t.Fatalf("a.go = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "b.go")); !strings.Contains(got, "TotalCents(1, 2)") {
		t.Fatalf("b.go = %q", got)
	}
	if _, err := r.Call(ctx, "ast_edit", map[string]any{"ops": []any{map[string]any{"pattern": "old_fn($X)", "rewrite": "new_fn($X)"}}, "paths": []any{"c.py"}, "lang": "python"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "c.py")); got != "x = new_fn(1)\n" {
		t.Fatalf("c.py = %q", got)
	}
	if _, err := r.Call(ctx, "ast_edit", map[string]any{"ops": []any{map[string]any{"pattern": "x", "rewrite": "y"}}, "paths": []any{"../out.go"}}); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("outside ast_edit err = %v", err)
	}
}

package workspace

import (
	"context"
	"fmt"
	"testing"
	"testing/fstest"
)

func TestIOFSOverMemoryAndOS(t *testing.T) {
	ctx := context.Background()
	for name, fsys := range map[string]FS{"memory": NewMemory(), "os": OS(t.TempDir())} {
		t.Run(name, func(t *testing.T) {
			if err := fsys.MkdirAll(ctx, "dir/sub", 0o755); err != nil {
				t.Fatal(err)
			}
			for p, body := range map[string]string{"dir/a.txt": "hello", "dir/sub/b.txt": "world", "top.txt": ""} {
				if err := fsys.WriteFile(ctx, p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := fstest.TestFS(IOFS(ctx, fsys), "dir/a.txt", "dir/sub/b.txt", "top.txt"); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Rename(ctx, "dir/sub", "moved"); err != nil {
				t.Fatal(err)
			}
			if got, err := fsys.ReadFile(ctx, "moved/b.txt"); err != nil || string(got) != "world" {
				t.Fatalf("after rename: %q, %v", got, err)
			}
			if _, err := fsys.ReadFile(ctx, "../escape"); err == nil {
				t.Fatal("read outside the root")
			}
		})
	}
}

func TestMemoryIsolation(t *testing.T) {
	ctx := context.Background()
	parent := NewMemory()
	_ = parent.MkdirAll(ctx, "src", 0o755)
	_ = parent.WriteFile(ctx, "src/a.go", []byte("a"), 0o644)
	_ = parent.WriteFile(ctx, "src/b.go", []byte("b"), 0o644)

	// A clean merge: the worker's changes land in the parent.
	child, finish, err := parent.Isolate(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = child.FS.WriteFile(ctx, "src/a.go", []byte("A"), 0o644)
	_ = child.FS.WriteFile(ctx, "src/new.go", []byte("new"), 0o644)
	_ = child.FS.Remove(ctx, "src/b.go")
	if got, _ := parent.ReadFile(ctx, "src/a.go"); string(got) != "a" {
		t.Fatal("worker's write reached the parent before finish")
	}
	_ = parent.WriteFile(ctx, "other.txt", []byte("parent work"), 0o644) // unrelated path
	res, err := finish(ctx, true)
	if err != nil || !res.Merged || len(res.Changed) != 3 {
		t.Fatalf("merge = %+v, %v", res, err)
	}
	if got, _ := parent.ReadFile(ctx, "src/a.go"); string(got) != "A" {
		t.Fatalf("a.go = %q", got)
	}
	if _, err := parent.Stat(ctx, "src/b.go"); err == nil {
		t.Fatal("removed file survived the merge")
	}
	if got, _ := parent.ReadFile(ctx, "other.txt"); string(got) != "parent work" {
		t.Fatal("merge lost the parent's own change")
	}

	// A conflict: nothing merges and the copy is kept.
	child, finish, _ = parent.Isolate(ctx, "job-2")
	_ = child.FS.WriteFile(ctx, "src/a.go", []byte("worker"), 0o644)
	_ = parent.WriteFile(ctx, "src/a.go", []byte("parent"), 0o644)
	res, err = finish(ctx, true)
	if err != nil || res.Merged || len(res.Conflicting) != 1 || res.Conflicting[0] != "src/a.go" || res.Kept == "" {
		t.Fatalf("conflict = %+v, %v", res, err)
	}
	if got, _ := parent.ReadFile(ctx, "src/a.go"); string(got) != "parent" {
		t.Fatalf("conflicting merge changed the parent: %q", got)
	}
}

func TestMemoryChangeTracking(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	_ = m.WriteFile(ctx, "keep.txt", []byte("same"), 0o644)
	_ = m.WriteFile(ctx, "edit.txt", []byte("before"), 0o644)
	_ = m.WriteFile(ctx, "gone.txt", []byte("x"), 0o644)
	mark, err := m.Mark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.WriteFile(ctx, "edit.txt", []byte("after"), 0o644)
	_ = m.Remove(ctx, "gone.txt")
	_ = m.MkdirAll(ctx, "d", 0o755)
	_ = m.WriteFile(ctx, "d/new.txt", []byte("new"), 0o644)
	changes, err := m.ChangedSince(ctx, mark)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.Path+":"+string(c.Before)+">"+string(c.After))
	}
	want := "[d/new.txt:>new edit.txt:before>after gone.txt:x>]"
	if fmt.Sprint(got) != want {
		t.Fatalf("changes = %v, want %s", got, want)
	}
	if changes[0].Before != nil || changes[2].After != nil {
		t.Fatal("added file should have nil Before and removed file nil After")
	}
	_ = m.Release(ctx, mark)
	if _, err := m.ChangedSince(ctx, mark); err == nil {
		t.Fatal("released mark still usable")
	}
}

func TestConflicts(t *testing.T) {
	got := Conflicts([]string{"a/b.txt", "c", "d/e"}, []string{"a/b.txt", "c/x", "d"})
	if fmt.Sprint(got) != "[a/b.txt c d/e]" {
		t.Fatalf("Conflicts = %v", got)
	}
	if got := Conflicts([]string{"ab"}, []string{"a"}); len(got) != 0 {
		t.Fatalf("name prefix treated as a conflict: %v", got)
	}
}

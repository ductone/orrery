package workspace

import (
	"context"
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

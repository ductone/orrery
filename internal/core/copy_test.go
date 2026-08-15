package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreePreservesDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(src, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dst, "linked"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("info=%v error=%v", info, err)
	}
}

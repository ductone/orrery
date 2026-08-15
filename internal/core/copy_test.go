package core

import (
	"context"
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

func TestCopyTreePrunesDependencies(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "vendor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "vendor", "large"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "vendor")); !os.IsNotExist(err) {
		t.Fatalf("vendor copied: %v", err)
	}
}

func TestPrepareSharedReadOnlyUsesExistingCheckout(t *testing.T) {
	root := t.TempDir()
	got, isolation, err := prepareWorkspace(context.Background(), root, filepath.Join(t.TempDir(), "job"), "shared-ro")
	if err != nil || got != root || isolation != "shared-ro" {
		t.Fatalf("path=%q isolation=%q error=%v", got, isolation, err)
	}
}

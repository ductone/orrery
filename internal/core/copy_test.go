package core

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceModesAreSemantic(t *testing.T) {
	for _, mode := range []string{"read", "shared-write"} {
		if err := validateWorkspaceMode(mode); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	for _, mode := range []string{"", "worktree", "copy", "shared-ro", "isolated-write"} {
		if err := validateWorkspaceMode(mode); err == nil {
			t.Fatalf("legacy or unknown mode %q accepted", mode)
		}
	}
}

func TestWorkspaceWriterLease(t *testing.T) {
	e := &Engine{writers: map[string]string{}}
	workspace := t.TempDir()
	release, err := e.acquireWorkspaceWriter(workspace, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.acquireWorkspaceWriter(filepath.Join(workspace, "."), "two"); err == nil || !strings.Contains(err.Error(), "active writer") {
		t.Fatalf("second writer error = %v", err)
	}
	release()
	secondRelease, err := e.acquireWorkspaceWriter(workspace, "two")
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}

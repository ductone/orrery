package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/store"
	builtin "github.com/ductone/orrery/internal/tools"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ageDir backdates a directory past scratchTTL.
func ageDir(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-2 * scratchTTL)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(dir string) bool { _, err := os.Stat(dir); return err == nil }

func TestSharedScratchRootSparesOtherEngines(t *testing.T) {
	// The default root is os.TempDir()/orrery.
	t.Setenv("TMPDIR", t.TempDir())
	root := builtin.ScratchRoot()
	a := New(config.Config{}, openStore(t), nil, nil)
	liveA := a.scratchDir("session-of-a")
	mkdir(t, liveA)
	ageDir(t, liveA)

	b := New(config.Config{}, openStore(t), nil, nil)
	if !exists(liveA) {
		t.Fatalf("engine B removed engine A's scratch dir in %s", root)
	}

	// A recently finished session of B's own store is kept until it is old.
	st := openStore(t)
	addSession(t, st, "finished", "", "pass")
	known := filepath.Join(root, "finished")
	mkdir(t, known)
	ageDir(t, known)
	New(config.Config{}, st, nil, nil)
	if !exists(known) {
		t.Fatal("scratch of a recently finished session was removed")
	}
	_ = b
}

func TestScratchRootOptionOwnsItsDirectory(t *testing.T) {
	root := t.TempDir()
	unknownOld := filepath.Join(root, "unknown-old")
	unknownNew := filepath.Join(root, "unknown-new")
	mkdir(t, unknownOld)
	mkdir(t, unknownNew)
	ageDir(t, unknownOld)

	e := New(config.Config{}, openStore(t), nil, nil, WithScratchRoot(root))
	if exists(unknownOld) {
		t.Fatal("old unknown directory in an owned root was kept")
	}
	if !exists(unknownNew) {
		t.Fatal("recent unknown directory was removed")
	}
	if got := e.scratchDir("s1"); got != filepath.Join(root, "s1") {
		t.Fatalf("scratch dir = %s", got)
	}
}

func TestDeleteRemovesScratch(t *testing.T) {
	root := t.TempDir()
	st := openStore(t)
	e := New(config.Config{}, st, nil, nil, WithScratchRoot(root))
	addSession(t, st, "top", "", "pass")
	addSession(t, st, "worker", "top", "pass")
	mkdir(t, e.scratchDir("top"))
	ctx := context.Background()

	if err := e.Delete(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if !exists(e.scratchDir("top")) {
		t.Fatal("deleting a worker removed its parent's scratch dir")
	}
	if err := e.Delete(ctx, "top"); err != nil {
		t.Fatal(err)
	}
	if exists(e.scratchDir("top")) {
		t.Fatal("Delete left the session's scratch dir")
	}
}

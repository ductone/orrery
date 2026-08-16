package hashline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyAndStale(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	raw, _ := json.Marshal(ls[1])
	if string(raw) != "{\"number\":2,\"hash\":\"f44e64e7\",\"text\":\"beta\"}" {
		t.Fatalf("bad JSON %s", raw)
	}
	if err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[1].Hash, Delete: 1, Insert: []string{"BETA"}}}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("bad patch %q", b)
	}
	err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[1].Hash, Delete: 1}}})
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("expected stale, got %v", err)
	}
}

func TestApplyRejectsAccidentalDeclarationDeletion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ts")
	if err := os.WriteFile(p, []byte("const theme = makeTheme()\nuse(theme)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[0].Hash, Delete: 1, Insert: []string{"use(theme)"}}}})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete declaration") {
		t.Fatalf("error=%v", err)
	}
	if err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[0].Hash, Delete: 1, Insert: nil, AllowStructuralChange: true}}}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRejectsNoOpPatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lines, _ := Read(p)
	err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: lines[0].Hash, Delete: 1, Insert: []string{"unchanged"}}}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
}

func TestAmbiguousAnchorRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(p, []byte("same\nsame\n"), 0600)
	ls, _ := Read(p)
	err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[0].Hash, Delete: 1}}})
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("expected ambiguity: %v", err)
	}
}

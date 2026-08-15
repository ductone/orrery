package hashline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

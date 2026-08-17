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
	if _, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[1].Hash, Delete: 1, Insert: []string{"BETA"}}}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("bad patch %q", b)
	}
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[1].Hash, Delete: 1}}})
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
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[0].Hash, Delete: 1, Insert: []string{"use(theme)"}}}})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete declaration") {
		t.Fatalf("error=%v", err)
	}
	if _, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[0].Hash, Delete: 1, Insert: nil, AllowStructuralChange: true}}}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRejectsNoOpPatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lines, _ := Read(p)
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: lines[0].Hash, Delete: 1, Insert: []string{"unchanged"}}}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
}

func TestApplyCreatesNewFileFromEmptyAnchor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new.go")
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: hash(""), Insert: []string{"package sample", "", "const value = 1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "package sample\n\nconst value = 1\n" {
		t.Fatalf("content=%q error=%v", b, err)
	}
}

func TestAmbiguousAnchorRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(p, []byte("same\nsame\n"), 0600)
	ls, _ := Read(p)
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{{Anchor: ls[0].Hash, Delete: 1}}})
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("expected ambiguity: %v", err)
	}
}

func TestApplyMultiHunkAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{
		{Anchor: ls[1].Hash, Delete: 1, Insert: []string{"B"}},
		{Anchor: ls[2].Hash, Delete: 0, Insert: []string{"X"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	// Insert-before convention: X lands before the anchor line c. Both hunks
	// resolve against the pre-edit snapshot, so hunk1's edit does not shift hunk2.
	if string(b) != "a\nB\nX\nc\nd\n" {
		t.Fatalf("bad multi-hunk result %q", b)
	}
}

func TestApplyAllOrNothingInvalidHunk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{
		{Anchor: ls[1].Hash, Delete: 1, Insert: []string{"B"}},
		{Anchor: "deadbeef", Delete: 1, Insert: []string{"Z"}},
	}})
	if err == nil {
		t.Fatal("expected error for invalid hunk")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "a\nb\nc\nd\n" {
		t.Fatalf("file was partially modified: %q", b)
	}
}

func TestApplyResultFreshAnchors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	res, err := Apply(Patch{Path: p, Hunks: []Hunk{
		{Anchor: ls[1].Hash, Delete: 1, Insert: []string{"BETA"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Lines) == 0 {
		t.Fatalf("expected fresh lines, got %v", res)
	}
	found := false
	for _, ln := range res.Lines {
		if ln.Text == "BETA" {
			found = true
		}
		if ln.Number <= 0 || ln.Hash == "" {
			t.Fatalf("invalid line: %+v", ln)
		}
	}
	if !found {
		t.Fatalf("fresh lines missing inserted text: %+v", res.Lines)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("bad patch %q", b)
	}
}

func TestStaleErrorReturnsFreshWindow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{
		{Anchor: "deadbeef", Delete: 1},
	}})
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("expected stale error, got %v", err)
	}
	if len(stale.Fresh) == 0 {
		t.Fatal("expected fresh window in StaleError")
	}
	for _, ln := range stale.Fresh {
		if ln.Number <= 0 || ln.Hash == "" || ln.Text == "" {
			t.Fatalf("invalid fresh window line: %+v", ln)
		}
	}
}

func TestApplyAutoRebaseFuzzyAnchor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	// File has duplicate 'c' lines at indices 0 and 3
	if err := os.WriteFile(p, []byte("c\na\nb\nc\nd\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	// Model wants to insert 'X' before the second 'c' (index 3)
	// Uses anchor 'c' with offset=-1, which targets index 2
	// First occurrence at index 0 gives target -1 (invalid)
	// Auto-rebase should relocate to index 3, giving target 2
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{
		{Anchor: ls[0].Hash, Offset: -1, Delete: 0, Insert: []string{"X"}},
	}})
	if err != nil {
		t.Fatalf("auto-rebase failed: %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "c\na\nX\nb\nc\nd\n" {
		t.Fatalf("bad rebase result %q", b)
	}
}

func TestApplyAutoRebaseDoesNotDeleteFuzzyAnchor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	// File has duplicate 'c' lines at indices 0 and 3
	if err := os.WriteFile(p, []byte("c\na\nb\nc\nd\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ls, _ := Read(p)
	// Model wants to delete at the second 'c' (index 3)
	// Uses anchor 'c' with offset=-1, which targets index 2
	// First occurrence at index 0 gives target -1 (invalid)
	// Auto-rebase would relocate to index 3, but delete hunks are never fuzzy-applied
	_, err := Apply(Patch{Path: p, Hunks: []Hunk{
		{Anchor: ls[0].Hash, Offset: -1, Delete: 1, Insert: []string{"X"}},
	}})
	if err == nil {
		t.Fatal("expected error for fuzzy delete")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "c\na\nb\nc\nd\n" {
		t.Fatalf("file was modified despite fuzzy delete: %q", b)
	}
}

func TestContextualAnchorsDisambiguateRepeatedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(p, []byte("first\n}\nleft\nsecond\n}\nright\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := ReadWithMode(p, AnchorContextual)
	if err != nil {
		t.Fatal(err)
	}
	if lines[1].Hash == lines[4].Hash {
		t.Fatalf("repeated lines retained the same contextual hash: %q", lines[1].Hash)
	}
	if _, err := ApplyWithMode(Patch{Path: p, Hunks: []Hunk{{Anchor: lines[4].Hash, Delete: 1, Insert: []string{"} // target"}}}}, AnchorContextual); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "second\n} // target\nright") {
		t.Fatalf("wrong repeated line edited: %q", b)
	}
}

func TestContextualAnchorBecomesStaleWhenNeighborChanges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("before\ntarget\nafter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := ReadWithMode(p, AnchorContextual)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("changed\ntarget\nafter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyWithMode(Patch{Path: p, Hunks: []Hunk{{Anchor: lines[1].Hash, Delete: 1, Insert: []string{"updated"}}}}, AnchorContextual)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("neighbor change did not stale the anchor: %v", err)
	}
}

func TestTextAnchorDialectUsesExactLineText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := ReadWithMode(p, AnchorText)
	if err != nil {
		t.Fatal(err)
	}
	if lines[1].Hash != "beta" {
		t.Fatalf("text anchor=%q", lines[1].Hash)
	}
	if _, err := ApplyWithMode(Patch{Path: p, Hunks: []Hunk{{Anchor: "beta", Delete: 1, Insert: []string{"BETA"}}}}, AnchorText); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("content=%q", b)
	}
}

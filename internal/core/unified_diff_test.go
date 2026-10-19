package core

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiffFormat(t *testing.T) {
	got := string(unifiedDiff("f.txt", []byte("a\nb\nc\n"), []byte("a\nB\nc\n"), 3))
	want := "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"
	if got != want {
		t.Fatalf("diff =\n%s\nwant\n%s", got, want)
	}
}

// TestUnifiedDiffAppliesWithGit checks the diffs are valid patches: git
// applies each one to the before file and produces the after file.
func TestUnifiedDiffAppliesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	var long []string
	for i := range 200 {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	edited := append([]string(nil), long...)
	edited[10] = "changed 10"
	edited = append(edited[:100], append([]string{"inserted"}, edited[100:]...)...)
	edited = append(edited[:150], edited[152:]...)
	cases := []struct{ before, after []byte }{
		{[]byte(strings.Join(long, "\n") + "\n"), []byte(strings.Join(edited, "\n") + "\n")},
		{nil, []byte("new\nfile\n")},
		{[]byte("going\naway\n"), nil},
		{[]byte("x\n"), []byte("")},
	}
	for i, c := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if c.before != nil {
			if err := os.WriteFile(path, c.before, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		patch := unifiedDiff("f.txt", c.before, c.after, 3)
		cmd := exec.Command("git", "apply", "--unidiff-zero", "-")
		cmd.Dir = dir
		cmd.Stdin = bytes.NewReader(patch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("case %d: git apply: %v: %s\n%s", i, err, out, patch)
		}
		got, err := os.ReadFile(path)
		if c.after == nil {
			if err == nil {
				t.Fatalf("case %d: file not removed", i)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, c.after) {
			t.Fatalf("case %d: applied result %q, %v", i, got, err)
		}
	}
}

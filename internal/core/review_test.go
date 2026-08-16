package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectWorkspaceDiffIncludesNewFilesAndIgnoresRuntimeState(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.go")
	runGit("commit", "-qm", "fixture")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new_test.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".orrery"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".orrery", "runtime.log"), []byte("private runtime state"), 0o600); err != nil {
		t.Fatal(err)
	}

	diff, err := collectWorkspaceDiff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(diff)
	for _, want := range []string{"tracked.go", "new_test.go", "+package changed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "runtime.log") || strings.Contains(text, "private runtime state") {
		t.Fatalf("runtime state leaked into review diff:\n%s", text)
	}
	dirty, err := workspaceHasReviewableChanges(context.Background(), dir)
	if err != nil || !dirty {
		t.Fatalf("dirty=%v err=%v", dirty, err)
	}
}

func TestCollectWorkspaceDiffSupportsNonGitWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".orrery"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".orrery", "runtime.log"), []byte("omit me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dirty, err := workspaceHasReviewableChanges(context.Background(), dir); err != nil || dirty {
		t.Fatalf("non-git workspace was treated as an existing dirty checkout: dirty=%v err=%v", dirty, err)
	}
	diff, err := collectWorkspaceDiff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "result.txt") || !strings.Contains(string(diff), "+complete") || strings.Contains(string(diff), "runtime.log") {
		t.Fatalf("unexpected non-git review input:\n%s", diff)
	}
}

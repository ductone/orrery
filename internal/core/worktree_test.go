package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ductone/orrery/agentproto"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func startIso(t *testing.T, repo, name string) (string, string, string) {
	t.Helper()
	wt := filepath.Join(repo, ".orrery", "worktrees", name)
	branch := "orrery/job-" + name
	if err := createIsolatedWorktree(context.Background(), repo, wt, branch); err != nil {
		t.Fatal(err)
	}
	return wt, branch, gitOutput(context.Background(), wt, "rev-parse", "HEAD")
}

func TestIsolatedWorktreesMergeAndCleanUp(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	repo := newRepo(t)
	// Uncommitted parent change must be visible to the worker.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a-dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt1, br1, base1 := startIso(t, repo, "w1")
	wt2, br2, base2 := startIso(t, repo, "w2")
	if b, _ := os.ReadFile(filepath.Join(wt1, "a.txt")); string(b) != "a-dirty\n" {
		t.Fatalf("worker did not see parent changes: %q", b)
	}
	_ = os.WriteFile(filepath.Join(wt1, "new1.txt"), []byte("1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wt2, "new2.txt"), []byte("2\n"), 0o644)
	ctx := context.Background()
	r1 := finalizeIsolatedJob(ctx, repo, wt1, br1, base1, agentproto.TaskResult{Status: agentproto.Pass})
	r2 := finalizeIsolatedJob(ctx, repo, wt2, br2, base2, agentproto.TaskResult{Status: agentproto.Pass})
	for _, r := range []agentproto.TaskResult{r1, r2} {
		if r.Result["merged"] != true {
			t.Fatalf("expected clean merge: %v", r.Result)
		}
	}
	for _, f := range []string{"new1.txt", "new2.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("missing %s in parent: %v", f, err)
		}
	}
	if _, err := os.Stat(wt1); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed")
	}
}

func TestIsolatedWorktreeConflictReported(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	repo := newRepo(t)
	wt, br, base := startIso(t, repo, "c1")
	_ = os.WriteFile(filepath.Join(wt, "a.txt"), []byte("worker\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("parent\n"), 0o644)
	r := finalizeIsolatedJob(context.Background(), repo, wt, br, base, agentproto.TaskResult{Status: agentproto.Pass})
	if r.Result["merged"] != false || r.Result["branch"] != br {
		t.Fatalf("expected conflict report: %v", r.Result)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "parent\n" {
		t.Fatalf("parent modified on conflict: %q", b)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed after conflict")
	}
}

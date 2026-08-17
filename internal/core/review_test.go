package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/store"
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
func TestClassifyReviewJob(t *testing.T) {
	cases := []struct {
		name      string
		job       store.Job
		wantPass  bool
		wantErrIs error
	}{
		{
			name:     "pass true is a clean verdict",
			job:      store.Job{ID: "j1", Status: string(agentproto.Pass), ResultJSON: `{"pass":true,"findings":[]}`},
			wantPass: true,
		},
		{
			name:     "pass false is a genuine rejection",
			job:      store.Job{ID: "j2", Status: string(agentproto.Pass), ResultJSON: `{"pass":false,"findings":["bug"]}`},
			wantPass: false,
		},
		{
			name:      "missing pass verdict is inconclusive",
			job:       store.Job{ID: "j3", Status: string(agentproto.Pass), ResultJSON: `{"findings":[]}`},
			wantErrIs: ErrReviewInconclusive,
		},
		{
			name:      "invalid JSON is inconclusive",
			job:       store.Job{ID: "j4", Status: string(agentproto.Pass), ResultJSON: `not json`},
			wantErrIs: ErrReviewInconclusive,
		},
		{
			name:      "budget exhausted is inconclusive",
			job:       store.Job{ID: "j5", Status: string(agentproto.BudgetExhausted), ResultJSON: `{}`},
			wantErrIs: ErrReviewInconclusive,
		},
		{
			name:      "failed status is inconclusive",
			job:       store.Job{ID: "j6", Status: string(agentproto.Fail), ResultJSON: `{}`},
			wantErrIs: ErrReviewInconclusive,
		},
		{
			name:      "cancelled status is inconclusive",
			job:       store.Job{ID: "j7", Status: string(agentproto.Cancelled), ResultJSON: `{}`},
			wantErrIs: ErrReviewInconclusive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			passed, text, err := classifyReviewJob(tc.job)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("wanted error wrapping %v, got nil", tc.wantErrIs)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("wanted error wrapping %v, got %v", tc.wantErrIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if passed != tc.wantPass {
				t.Fatalf("passed=%v, want %v", passed, tc.wantPass)
			}
			if text == "" {
				t.Fatalf("expected non-empty review text")
			}
		})
	}
}

func TestReviewChildBudget(t *testing.T) {
	const fraction = 0.10
	cases := []struct {
		name      string
		parentMax float64
		available float64
		want      float64
	}{
		{"small parent budget gets floor", 1.00, 1.00, 0.50},
		{"large parent budget uses fraction", 10.00, 10.00, 1.00},
		{"available cap wins over floor", 10.00, 0.30, 0.30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := min(tc.parentMax*fraction, tc.available*fraction)
			got := reviewChildBudget(base, tc.available)
			if got != tc.want {
				t.Fatalf("reviewChildBudget(%v, %v) = %v, want %v", base, tc.available, got, tc.want)
			}
		})
	}
}

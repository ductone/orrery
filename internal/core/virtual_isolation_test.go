package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/workspace"
)

func TestFinalizeVirtualJob(t *testing.T) {
	ctx := context.Background()
	parent := workspace.NewMemory()
	_ = parent.WriteFile(ctx, "a.txt", []byte("a"), 0o644)

	child, finish, _ := parent.Isolate(ctx, "w1")
	_ = child.FS.WriteFile(ctx, "new.txt", []byte("1"), 0o644)
	r := finalizeVirtualJob(ctx, finish, agentproto.TaskResult{Status: agentproto.Pass})
	if r.Result["merged"] != true {
		t.Fatalf("clean merge: %v", r.Result)
	}
	if _, err := parent.ReadFile(ctx, "new.txt"); err != nil {
		t.Fatal("worker's file not merged")
	}

	child, finish, _ = parent.Isolate(ctx, "w2")
	_ = child.FS.WriteFile(ctx, "a.txt", []byte("worker"), 0o644)
	_ = parent.WriteFile(ctx, "a.txt", []byte("parent"), 0o644)
	r = finalizeVirtualJob(ctx, finish, agentproto.TaskResult{Status: agentproto.Pass})
	if r.Result["merged"] != false || r.Result["kept"] != "memory:w2" {
		t.Fatalf("conflict: %v", r.Result)
	}
	if got, _ := parent.ReadFile(ctx, "a.txt"); string(got) != "parent" {
		t.Fatalf("parent changed on conflict: %q", got)
	}
}

// TestIsolatedWorkerInVirtualWorkspace has a parent spawn an isolated worker
// in an in-memory workspace. The worker writes a file in its own copy, the
// parent waits for it, and the file is merged into the parent's workspace.
func TestIsolatedWorkerInVirtualWorkspace(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mem := workspace.NewMemory()

	jobURI := regexp.MustCompile(`job://([0-9a-f-]{36})/result`)
	s := &scriptedResponses{reply: func(_ int, body map[string]any) map[string]any {
		raw, _ := json.Marshal(body)
		text := string(raw)
		outputs := strings.Count(text, `"function_call_output"`)
		if !strings.Contains(text, "PARENT-SPEC") {
			// The worker: write a file, then finish.
			if outputs == 0 {
				return responsesCall("w-1", "write", map[string]any{"path": "from-worker.md", "content": "worker output"})
			}
			return responsesText("worker done")
		}
		switch outputs {
		case 0:
			return responsesCall("p-1", "spawn", map[string]any{"spec": "WORKER-SPEC write a file", "workspace_mode": "isolated", "budget_fraction": 0.5})
		case 1:
			m := jobURI.FindStringSubmatch(text)
			if m == nil {
				return responsesText("no job id")
			}
			return responsesCall("p-2", "job", map[string]any{"id": m[1], "action": "wait"})
		default:
			return responsesText("parent done")
		}
	}}
	srv := s.serve(t)
	cfg := gateConfig(t.TempDir(), srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 5, JobDefaultFraction: .5}
	e := New(cfg, st, provider.New(cfg), nil)
	t.Cleanup(func() { e.Close() })
	e.UseWorkspaces(func(context.Context, string, agentproto.Workspace) (workspace.Workspace, error) {
		return workspace.Virtual(mem), nil
	})

	req := agentproto.TaskRequest{Spec: "PARENT-SPEC delegate a file", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute, MaxDepth: 2}, Workspace: agentproto.Workspace{Path: "mem://agent-3", Mode: "shared-write", Ownership: "external"}}
	res, err := e.Run(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Pass {
		t.Fatalf("result = %+v", res)
	}
	got, err := mem.ReadFile(ctx, "from-worker.md")
	if err != nil || !strings.Contains(string(got), "worker output") {
		t.Fatalf("worker's file in the parent workspace = %q, %v", got, err)
	}
	sessions, _ := st.Sessions(ctx)
	jobs, _ := st.Jobs(ctx, sessions[0].ID)
	if len(jobs) != 1 || !strings.Contains(jobs[0].ResultJSON, `"merged":true`) || !strings.Contains(jobs[0].WorkspaceJSON, "shared-write") {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// TestReviewDiffInVirtualWorkspace checks review sees exactly what the run
// changed in a virtual workspace, as a unified diff.
func TestReviewDiffInVirtualWorkspace(t *testing.T) {
	ctx := context.Background()
	e, _ := testEngine(t)
	mem := workspace.NewMemory()
	_ = mem.WriteFile(ctx, "untouched.txt", []byte("same\n"), 0o644)
	_ = mem.WriteFile(ctx, "edited.txt", []byte("one\ntwo\n"), 0o644)
	mark, err := mem.Mark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.setSessionWorkspace("s", workspace.Virtual(mem))
	e.changeMarks = map[string]string{"s": mark}

	_ = mem.WriteFile(ctx, "edited.txt", []byte("one\nTWO\n"), 0o644)
	_ = mem.WriteFile(ctx, "added.txt", []byte("hello\n"), 0o644)
	diff, paths, err := e.reviewDiff(ctx, "s", "mem://agent")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "added.txt,edited.txt" {
		t.Fatalf("paths = %v", paths)
	}
	for _, want := range []string{"+++ b/added.txt", "+hello", "-two", "+TWO"} {
		if !strings.Contains(string(diff), want) {
			t.Fatalf("diff lacks %q:\n%s", want, diff)
		}
	}
	if strings.Contains(string(diff), "untouched.txt") {
		t.Fatal("diff includes an unchanged file")
	}
}

package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/workspace"
)

// TestWorkspaceWithoutFiles runs a task with no workspace files: the model is
// offered no file tools.
func TestWorkspaceWithoutFiles(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	srv := s.serve(t)
	cfg := gateConfig(t.TempDir(), srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 5, JobDefaultFraction: .2}
	e := New(cfg, st, provider.New(cfg), nil)
	t.Cleanup(func() { e.Close() })
	e.UseWorkspaces(func(context.Context, string, agentproto.Workspace) (workspace.Workspace, error) {
		return workspace.Workspace{}, nil
	})
	req := agentproto.TaskRequest{Spec: "answer a question", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: "none://agent-2", Mode: "shared-write", Ownership: "external"}}
	res, err := e.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Pass {
		t.Fatalf("result = %+v", res)
	}
	s.mu.Lock()
	body, _ := json.Marshal(s.requests[0])
	s.mu.Unlock()
	for _, tool := range []string{"read", "write", "edit", "search", "exec"} {
		if strings.Contains(string(body), `"name":"`+tool+`"`) {
			t.Fatalf("%s was offered without workspace files", tool)
		}
	}
}

// TestVirtualWorkspaceRun runs a task whose workspace exists only in memory:
// the model's write lands in the workspace's FS, instructions are discovered
// from it, and no exec tool is offered.
func TestVirtualWorkspaceRun(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	mem := workspace.NewMemory()
	if err := mem.WriteFile(ctx, "AGENTS.md", []byte("Always sign notes with ORRERY-VFS."), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n == 1 {
			return responsesCall("call-1", "write", map[string]any{"path": "note.md", "content": "hello from the agent"})
		}
		return responsesText("done")
	}}
	srv := s.serve(t)
	cfg := gateConfig(t.TempDir(), srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 5, JobDefaultFraction: .2}
	e := New(cfg, st, provider.New(cfg), nil)
	t.Cleanup(func() { e.Close() })
	var asked []string
	e.UseWorkspaces(func(_ context.Context, _ string, w agentproto.Workspace) (workspace.Workspace, error) {
		asked = append(asked, w.Path)
		return workspace.Virtual(mem), nil
	})

	req := agentproto.TaskRequest{Spec: "write a note", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: "mem://agent-1", Mode: "shared-write", Ownership: "external"}}
	res, err := e.Run(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Pass {
		t.Fatalf("result = %+v", res)
	}
	if len(asked) == 0 || asked[0] != "mem://agent-1" {
		t.Fatalf("workspace func asked for %v", asked)
	}
	got, err := mem.ReadFile(ctx, "note.md")
	if err != nil || !strings.Contains(string(got), "hello from the agent") {
		t.Fatalf("note.md = %q, %v", got, err)
	}

	s.mu.Lock()
	first := s.requests[0]
	s.mu.Unlock()
	body, _ := json.Marshal(first)
	if !strings.Contains(string(body), "ORRERY-VFS") {
		t.Fatal("AGENTS.md from the virtual workspace was not in the prompt")
	}
	if strings.Contains(string(body), `"name":"exec"`) {
		t.Fatal("exec was offered in a virtual workspace")
	}
	if !strings.Contains(string(body), `"name":"write"`) {
		t.Fatal("write was not offered")
	}
}

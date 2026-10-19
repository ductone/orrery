package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/google/uuid"
)

func hostToolNames(body map[string]any) []string {
	var names []string
	tools, _ := body["tools"].([]any)
	for _, t := range tools {
		m, _ := t.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	return names
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// hostToolEngine runs a scripted session on an engine built with opts. The
// model calls call (if non-empty) on its first turn and then finishes; it
// returns the first request body and the result.
func hostToolEngine(t *testing.T, mode, parentJob, call string, opts ...Option) (map[string]any, agentproto.TaskResult) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := New(config.Config{}, st, nil, nil, opts...)
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n == 1 && call != "" {
			return responsesCall("c1", call, map[string]any{"q": "hi"})
		}
		return responsesText("done")
	}}
	srv := s.serve(t)
	cfg := gateConfig(t.TempDir(), srv.URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{
		Spec:      "Do it",
		Budget:    agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute},
		Workspace: agentproto.Workspace{Path: cfg.WorkspaceRoot, Mode: mode, Ownership: "external"},
	}
	var result agentproto.TaskResult
	if mode == "read" || parentJob != "" {
		sid := uuid.NewString()
		if err := st.CreateSession(context.Background(), store.Session{ID: sid, Spec: req.Spec, Phase: "explore", BudgetUSD: 5, WorkspacePath: cfg.WorkspaceRoot, WorkspaceOwnership: "external"}); err != nil {
			t.Fatal(err)
		}
		req.Depth = 1
		result = e.run(context.Background(), sid, parentJob, req, nil)
	} else {
		_, results, err := e.Start(context.Background(), req, nil)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case result = <-results:
		case <-time.After(30 * time.Second):
			t.Fatal("run did not finish")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("no request reached the model")
	}
	return s.requests[0], result
}

func echoTool(name string, readOnly, workers bool, got *struct {
	mu   sync.Mutex
	sid  string
	args map[string]any
}) Tool {
	return Tool{
		Name: name, Description: "echo", InputSchema: map[string]any{"type": "object"},
		ReadOnly: readOnly, Workers: workers,
		Handler: func(_ context.Context, c ToolCall) (any, error) {
			if got != nil {
				got.mu.Lock()
				got.sid, got.args = c.SessionID, c.Args
				got.mu.Unlock()
			}
			return map[string]any{"ok": true}, nil
		},
	}
}

func TestHostToolIsOfferedAndCalledWithSession(t *testing.T) {
	var got struct {
		mu   sync.Mutex
		sid  string
		args map[string]any
	}
	body, result := hostToolEngine(t, "shared-write", "", "lookup", WithTools(echoTool("lookup", false, false, &got)))
	if !containsName(hostToolNames(body), "lookup") {
		t.Fatalf("host tool not offered: %v", hostToolNames(body))
	}
	if result.Status != agentproto.Pass {
		t.Fatalf("result = %+v", result)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.sid == "" || got.args["q"] != "hi" {
		t.Fatalf("handler saw session %q args %v", got.sid, got.args)
	}
}

func TestHostToolFailureAndPanicReachTheModel(t *testing.T) {
	for name, h := range map[string]func(context.Context, ToolCall) (any, error){
		"fails":  func(context.Context, ToolCall) (any, error) { return nil, errors.New("boom") },
		"panics": func(context.Context, ToolCall) (any, error) { panic("kaboom") },
	} {
		_, result := hostToolEngine(t, "shared-write", "", "t", WithTools(Tool{Name: "t", Handler: h}))
		if result.Status != agentproto.Pass || result.Outcome.ToolErrors != 1 {
			t.Fatalf("%s: result = %+v", name, result)
		}
	}
}

func TestHostToolsHonorReadModeAndWorkers(t *testing.T) {
	tools := WithTools(
		echoTool("top_only", false, false, nil),
		echoTool("ro_top", true, false, nil),
		echoTool("worker_rw", false, true, nil),
		echoTool("worker_ro", true, true, nil),
	)
	names := func(mode, parent string) []string {
		body, _ := hostToolEngine(t, mode, parent, "", tools)
		return hostToolNames(body)
	}
	check := func(label string, got []string, want ...string) {
		t.Helper()
		for _, n := range []string{"top_only", "ro_top", "worker_rw", "worker_ro"} {
			if containsName(got, n) != containsName(want, n) {
				t.Errorf("%s: tool %s offered = %v, want %v", label, n, containsName(got, n), containsName(want, n))
			}
		}
	}
	check("top-level write", names("shared-write", ""), "top_only", "ro_top", "worker_rw", "worker_ro")
	check("worker write", names("shared-write", "job-1"), "worker_rw", "worker_ro")
	check("read mode, top level", names("read", ""), "ro_top", "worker_ro")
	check("read mode worker", names("read", "job-1"), "worker_ro")
}

func TestDisabledToolIsAbsentFromRequestAndPrompt(t *testing.T) {
	body, _ := hostToolEngine(t, "shared-write", "", "", WithToolFilter(func(name string) bool { return name != "search" && name != "spawn" }))
	names := hostToolNames(body)
	if containsName(names, "search") || containsName(names, "spawn") || !containsName(names, "read") {
		t.Fatalf("tools = %v", names)
	}
	instructions, _ := body["instructions"].(string)
	if strings.Contains(instructions, "WORKERS") {
		t.Fatal("prompt still has the WORKERS section without spawn")
	}
}

func TestFilteredToolCannotBeCalled(t *testing.T) {
	_, result := hostToolEngine(t, "shared-write", "", "search", WithToolFilter(func(name string) bool { return name != "search" }))
	if result.Outcome.ToolErrors != 1 {
		t.Fatalf("calling a filtered tool must fail: %+v", result)
	}
}

func TestHostToolNameCollisionIsRejected(t *testing.T) {
	h := func(context.Context, ToolCall) (any, error) { return nil, nil }
	for name, tools := range map[string][]Tool{
		"builtin":   {{Name: "search", Handler: h}},
		"duplicate": {{Name: "a", Handler: h}, {Name: "a", Handler: h}},
		"unnamed":   {{Handler: h}},
		"no func":   {{Name: "b"}},
	} {
		if err := ValidateTools(tools); err == nil {
			t.Errorf("%s: want an error", name)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: WithTools must panic", name)
				}
			}()
			WithTools(tools...)
		}()
	}
	if err := ValidateTools([]Tool{{Name: "mine", Handler: h}}); err != nil {
		t.Fatal(err)
	}
}

// Every name the engine can register must be reserved, or a host tool could
// shadow it.
func TestReservedToolNamesCoverTheRegistry(t *testing.T) {
	e, _ := testEngine(t)
	r := e.toolRegistry("s", "", agentproto.TaskRequest{Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "shared-write"}}, "", &instructionDiscovery{}, nil)
	for _, d := range r.Definitions() {
		if !containsName(reservedToolNames, d.Name) {
			t.Errorf("built-in tool %q is not in reservedToolNames", d.Name)
		}
	}
}

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestTurnModeRestrictions(t *testing.T) {
	var m turnMode
	if m.active() || !m.permits("read") {
		t.Fatal("an unconstrained turn allows every tool")
	}
	m.restrict("advance", "todo", "edit")
	if !m.permits("edit") || m.permits("read") || m.noCalls {
		t.Fatalf("subset restriction wrong: %+v", m)
	}
	m.restrict("finish now")
	if !m.noCalls || m.permits("todo") || !strings.Contains(m.unavailable("read"), "no tool calls") {
		t.Fatalf("the last restriction must win: %+v", m)
	}
	history := m.apply([]provider.Message{{Role: "user", Content: "task"}})
	last := history[len(history)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "advance") || !strings.Contains(last.Content, "finish now") || !strings.Contains(last.Content, "disabled") {
		t.Fatalf("directive = %+v", last)
	}
	var subset turnMode
	subset.restrict("edit only", "edit")
	if msg := subset.apply(nil)[0].Content; !strings.Contains(msg, "Only these tools may be called this turn: edit") {
		t.Fatalf("directive = %q", msg)
	}
	if got := (&turnMode{}).apply([]provider.Message{{Role: "user"}}); len(got) != 1 {
		t.Fatal("an inactive mode must not add a message")
	}
}

// scriptedResponses serves the engine's Responses calls from a handler that
// sees each decoded request body. Title-generation calls are answered apart.
type scriptedResponses struct {
	mu       sync.Mutex
	requests []map[string]any
	reply    func(n int, body map[string]any) map[string]any
}

func (s *scriptedResponses) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if instructions, _ := body["instructions"].(string); !strings.Contains(instructions, "TOOL CALL DISCIPLINE") {
			_ = json.NewEncoder(w).Encode(responsesText("Title"))
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, body)
		n := len(s.requests)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s.reply(n, body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func responsesText(text string) map[string]any {
	return map[string]any{"model": "gpt-5.6-terra", "status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
}

func responsesCall(id, name string, args map[string]any) map[string]any {
	b, _ := json.Marshal(args)
	return map[string]any{"model": "gpt-5.6-terra", "status": "completed", "output": []any{map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(b)}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
}

func runScripted(t *testing.T, s *scriptedResponses, mode string) agentproto.TaskResult {
	t.Helper()
	e, _ := testEngine(t)
	workspace := t.TempDir()
	for i := range 8 {
		_ = os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%d.txt", i)), []byte(fmt.Sprintf("file %d\n", i)), 0600)
	}
	srv := s.serve(t)
	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
		Interventions: config.InterventionConfig{JudgeEnabled: new(bool)},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{
		Spec:      "Summarise the files",
		Budget:    agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute},
		Workspace: agentproto.Workspace{Path: workspace, Mode: mode, Ownership: "external"},
	}
	if mode == "read" {
		// Read workers are children; run one the way spawn does.
		sid := uuid.NewString()
		if err := e.store.CreateSession(context.Background(), store.Session{ID: sid, Spec: req.Spec, Phase: "explore", BudgetUSD: 5, WorkspacePath: workspace, WorkspaceOwnership: "external"}); err != nil {
			t.Fatal(err)
		}
		return e.run(context.Background(), sid, "job-1", req, nil)
	}
	_, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	return agentproto.TaskResult{}
}

func lastInputText(body map[string]any) string {
	input, _ := body["input"].([]any)
	if len(input) == 0 {
		return ""
	}
	last, _ := input[len(input)-1].(map[string]any)
	text, _ := last["content"].(string)
	return text
}

// A read worker forced to synthesise keeps its tool definitions and its
// instructions byte for byte, so the cached prefix survives; the directive
// rides as a trailing message and calls are forbidden with tool_choice none.
func TestForcedSynthesisKeepsPrefixAndForbidsCalls(t *testing.T) {
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		if body["tool_choice"] == "none" {
			return responsesText("summary of the files")
		}
		return responsesCall(fmt.Sprintf("c%d", n), "read", map[string]any{"path": fmt.Sprintf("f%d.txt", n)})
	}}
	result := runScripted(t, s, "read")
	if result.Status != agentproto.Pass {
		t.Fatalf("result = %+v", result)
	}
	first, forced := s.requests[0], s.requests[len(s.requests)-1]
	if forced["tool_choice"] != "none" {
		t.Fatal("the final request must forbid tool calls")
	}
	if len(forced["tools"].([]any)) != len(first["tools"].([]any)) {
		t.Fatalf("tool definitions changed: %d -> %d", len(first["tools"].([]any)), len(forced["tools"].([]any)))
	}
	if forced["instructions"] != first["instructions"] {
		t.Fatal("instructions must not change when a turn is constrained")
	}
	if text := lastInputText(forced); !strings.Contains(text, "HARNESS DIRECTIVE") || !strings.Contains(text, "Synthesize") {
		t.Fatalf("directive missing from the tail: %q", text)
	}
}

func TestToolCallsAreRefusedWhenForbidden(t *testing.T) {
	refused := false
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		if body["tool_choice"] == "none" {
			input, _ := body["input"].([]any)
			for _, raw := range input {
				item, _ := raw.(map[string]any)
				if out, _ := item["output"].(string); strings.Contains(out, "unavailable this turn") {
					refused = true
					return responsesText("done")
				}
			}
			// Ignore the constraint once, as a model occasionally does.
			return responsesCall(fmt.Sprintf("x%d", n), "read", map[string]any{"path": "f0.txt"})
		}
		return responsesCall(fmt.Sprintf("c%d", n), "read", map[string]any{"path": fmt.Sprintf("f%d.txt", n)})
	}}
	if result := runScripted(t, s, "read"); result.Status != agentproto.Pass || !refused {
		t.Fatalf("result=%+v refused=%v", result, refused)
	}
}

func TestTruncatedEmptyResponseRetriesWithMoreRoom(t *testing.T) {
	var caps []float64
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		caps = append(caps, body["max_output_tokens"].(float64))
		if n == 1 {
			return map[string]any{"model": "gpt-5.6-terra", "status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{map[string]any{"type": "reasoning", "summary": []any{}}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 8000}}
		}
		return responsesText("finished")
	}}
	result := runScripted(t, s, "shared-write")
	if result.Status != agentproto.Pass {
		t.Fatalf("result = %+v", result)
	}
	if len(caps) != 2 || caps[0] != defaultOutputCap || caps[1] != 2*defaultOutputCap {
		t.Fatalf("output caps = %v", caps)
	}
	for _, req := range s.requests[1:] {
		if strings.Contains(lastInputText(req), "was empty") {
			t.Fatal("a truncated reply must not be answered with the empty-response nudge")
		}
	}
}

func TestEmptyResponsesReportTheStopReason(t *testing.T) {
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any {
		return map[string]any{"model": "gpt-5.6-terra", "status": "completed", "output": []any{map[string]any{"type": "reasoning", "summary": []any{}}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 50}}
	}}
	result := runScripted(t, s, "shared-write")
	if result.Status != agentproto.Fail || !strings.Contains(result.Error, `"completed"`) || !strings.Contains(result.Error, "reasoning") {
		t.Fatalf("result = %+v", result)
	}
}

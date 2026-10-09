package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestNormalWorkTurnModesAreAdvisory(t *testing.T) {
	for _, phase := range []string{"explore", "plan", "implement", "review", "diagnose"} {
		t.Run(phase, func(t *testing.T) {
			e, st := testEngine(t)
			workspace := t.TempDir()
			advised, searched := false, false
			s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
				if body["tool_choice"] == "none" {
					t.Error("normal work must retain tool calls")
				}
				input, _ := body["input"].([]any)
				for _, raw := range input {
					item, _ := raw.(map[string]any)
					if out, _ := item["output"].(string); strings.Contains(out, "unavailable this turn") {
						t.Error("advice refused a tool")
					}
				}
				if n <= 10 {
					return responsesCall(fmt.Sprintf("e%d", n), "exec", map[string]any{"command": "printf working"})
				}
				if n == 11 {
					if !strings.Contains(lastInputText(body), "Consider") && !strings.Contains(lastInputText(body), "consider") {
						t.Errorf("advice missing: %q", lastInputText(body))
					}
					return responsesCall("search-after-advice", "search", map[string]any{"pattern": "nothing"})
				}
				return responsesText("Finished the requested investigation.")
			}}
			srv := s.serve(t)
			cfg := gateConfig(workspace, srv.URL)
			e.ReplaceRuntime(cfg, provider.New(cfg), nil)
			sid := uuid.NewString()
			ctx := context.Background()
			if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "Investigate", Phase: phase, BudgetUSD: 5, WorkspacePath: workspace}); err != nil {
				t.Fatal(err)
			}
			req := agentproto.TaskRequest{Spec: "Investigate", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
			result := e.run(ctx, sid, "", req, func(event agentproto.AgentEvent) {
				if event.Type == "progress.intervention" {
					data := event.Data.(map[string]any)
					advised = true
					if data["kind"] != "mode.advise" || data["no_calls"] != false {
						t.Errorf("intervention = %+v", data)
					}
				}
				if event.Type == "tool.called" {
					searched = true
				}
			})
			if result.Status != agentproto.Pass || !advised {
				t.Fatalf("result=%+v advised=%v", result, advised)
			}
			// The search result, not an unavailable-tool refusal, must be in history.
			msgs, err := st.Messages(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			searched = false
			for _, m := range msgs {
				if m.Role == "tool" && strings.Contains(m.ContentJSON, "search-after-advice") {
					searched = true
				}
			}
			if !searched {
				t.Fatal("search after advice was not executed")
			}
			first := s.requests[0]
			for _, body := range s.requests[1:] {
				if len(body["tools"].([]any)) != len(first["tools"].([]any)) {
					t.Error("advice changed tool definitions")
				}
			}
		})
	}
}

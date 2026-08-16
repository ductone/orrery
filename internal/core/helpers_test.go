package core

import (
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func TestEmptyFinalResponse(t *testing.T) {
	if !emptyFinalResponse(provider.Message{Reasoning: "internal reasoning only"}) {
		t.Fatal("reasoning-only response must not complete a task")
	}
	if emptyFinalResponse(provider.Message{Content: "done"}) {
		t.Fatal("non-empty response was rejected")
	}
	if emptyFinalResponse(provider.Message{ToolCalls: []provider.ToolCall{{Name: "read"}}}) {
		t.Fatal("tool call response was rejected")
	}
}

func TestToolCallKeyIgnoresArgumentMapOrder(t *testing.T) {
	left := provider.ToolCall{Name: "todo", Arguments: map[string]any{"b": 2, "a": 1}}
	right := provider.ToolCall{Name: "todo", Arguments: map[string]any{"a": 1, "b": 2}}
	if toolCallKey(left) != toolCallKey(right) {
		t.Fatal("semantically identical tool calls must share a key")
	}
	right.Name = "edit"
	if toolCallKey(left) == toolCallKey(right) {
		t.Fatal("different tools must not share a key")
	}
}

func TestResultSchemaValidation(t *testing.T) {
	s := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []any{"ok"}, "additionalProperties": false}
	if err := validateSchema(s, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := validateSchema(s, map[string]any{"ok": "yes"}); err == nil {
		t.Fatal("wrong type accepted")
	}
	if err := validateSchema(s, map[string]any{"ok": true, "extra": 1}); err == nil {
		t.Fatal("extra accepted")
	}
}
func TestPlanSnapshotReplaced(t *testing.T) {
	one := setPlanSnapshot("summary", "one")
	two := setPlanSnapshot(one, "two")
	if strings.Contains(two, "one") || !strings.Contains(two, "two") || strings.Count(two, "[TODO SNAPSHOT]") != 1 {
		t.Fatal(two)
	}
}
func TestCompactionKeepsWholeAssistantTurn(t *testing.T) {
	msgs := []store.Message{{Role: "assistant"}, {Role: "tool"}, {Role: "assistant"}, {Role: "tool"}, {Role: "tool"}, {Role: "assistant"}, {Role: "tool"}, {Role: "assistant"}, {Role: "tool"}, {Role: "tool"}}
	at := compactionKeepIndex(msgs, 3)
	if at != 2 {
		t.Fatalf("cut at %d", at)
	}
	if msgs[at].Role != "assistant" {
		t.Fatal("orphaned tool result")
	}
}

func TestFallbackCompactionProducesRecoveryContract(t *testing.T) {
	s := store.Session{Spec: "Fix the retry race", DurableSummary: "previous constraints"}
	state := fallbackDurableState(s, []store.Message{
		{Role: "user", ContentJSON: `{"content":"Do not open a PR"}`},
		{Role: "tool", ContentJSON: `{"command":"go test ./...","ok":true}`},
		{Role: "assistant", ContentJSON: `{"content":"The race is in retry.go"}`},
	})
	if !state.valid() || state.Objective != s.Spec || len(state.Verification) == 0 || len(state.Decisions) == 0 || state.PriorSummary == "" {
		t.Fatalf("state=%+v", state)
	}
}

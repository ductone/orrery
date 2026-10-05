package core

import (
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func TestTranscriptProjectionKeepsMiddleFactsWithinBudget(t *testing.T) {
	user := provider.Message{Role: "user", Content: "MIDDLE USER FACT: preserve the blue constraint"}
	assistant := provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "1", Name: "exec", Arguments: map[string]any{"command": "printf middle"}}}}
	result := provider.Message{Role: "tool", ToolCallID: "1", Content: "edited internal/core/middle.go\n" + strings.Repeat("x", 20_000)}
	old := []store.Message{
		{Role: "tool", ContentJSON: store.JSON(provider.Message{Content: strings.Repeat("old", 20_000)})},
		{Role: "user", ContentJSON: store.JSON(user)},
		{Role: "assistant", ContentJSON: store.JSON(assistant)},
		{Role: "tool", ContentJSON: store.JSON(result)},
		{Role: "tool", ContentJSON: store.JSON(provider.Message{Content: strings.Repeat("new", 20_000)})},
	}
	got := strings.Join(transcriptChunks(old, 5_000), "\n")
	for _, fact := range []string{"MIDDLE USER FACT", "internal/core/middle.go", "edited internal/core/middle.go"} {
		if !strings.Contains(got, fact) {
			t.Fatalf("projection dropped %q", fact)
		}
	}
	if len(got) > 20_000 {
		t.Fatalf("projected transcript = %d chars", len(got))
	}
	if got := summaryTranscriptBudget(model.ModelSpec{ContextWindow: 100_000}); got != 50_000 {
		t.Fatalf("budget = %d", got)
	}
}

func TestMaskingAndRecentContextRetention(t *testing.T) {
	msgs := []store.Message{}
	for i := 0; i < 12; i++ {
		msgs = append(msgs, store.Message{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: "c", Name: "read"}}})})
		msgs = append(msgs, store.Message{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: "c", Content: "old output"})})
	}
	if !maskOldToolResults(msgs) || !strings.Contains(msgs[1].ContentJSON, "cleared") || strings.Contains(msgs[len(msgs)-1].ContentJSON, "cleared") {
		t.Fatal("old tool output was not masked in batch")
	}
	recent := []store.Message{{Role: "assistant", ContentJSON: "{}"}, {Role: "user", ContentJSON: store.JSON(provider.Message{Content: "latest"})}, {Role: "assistant", ContentJSON: "{}"}}
	if got := compactionKeepIndex(recent, 4); got != 1 {
		t.Fatalf("keep index = %d", got)
	}
}

func TestAuditAndRecallHistory(t *testing.T) {
	msgs := []store.Message{
		{Role: "user", ContentJSON: store.JSON(provider.Message{Content: "Preserve the blue constraint"})},
		{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: "e", Name: "edit", Arguments: map[string]any{"path": "internal/core/audit.go"}}}})},
		{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: "e", Content: "edited"})},
		{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: "x", Name: "exec", Arguments: map[string]any{"command": "go test ./internal/core"}}}})},
		{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: "x", Content: "ok"})},
	}
	state := auditDurableState(DurableState{}, msgs)
	if len(state.Instructions) == 0 || len(state.Files) == 0 || len(state.Verification) == 0 {
		t.Fatalf("audit dropped facts: %+v", state)
	}
	cp := store.Checkpoint{ID: "cp", MessagesJSON: store.JSON(msgs)}
	if got := recallHistory([]store.Checkpoint{cp}, "blue constraint"); len(got) != 1 || !strings.Contains(got[0], "cp#0") {
		t.Fatalf("recall = %v", got)
	}
}

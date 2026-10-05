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

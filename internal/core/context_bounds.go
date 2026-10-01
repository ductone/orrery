package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

// maxToolResultChars bounds one tool result in history (about 25K tokens).
// Built-in tools shape their own output, but a fetch of raw HTML once stored
// 2.7MB, and a handful of such results put the history beyond every model's
// context window. MCP tools are bounded the same way.
const (
	maxToolResultChars = 100_000
	toolResultHead     = 80_000
	toolResultTail     = 10_000
)

// capToolResult bounds a serialised tool result, keeping its head and tail
// and saying how to get the rest.
func capToolResult(tool, content string) string {
	if len(content) <= maxToolResultChars {
		return content
	}
	return store.JSON(map[string]any{
		"truncated":      true,
		"tool":           tool,
		"original_chars": len(content),
		"head":           content[:toolResultHead],
		"tail":           content[len(content)-toolResultTail:],
		"hint":           "This result was too large to keep in context. Narrow the call (a smaller range, a more specific query, a start offset) to see the omitted middle.",
	})
}

// shrinkStoredToolMessage applies capToolResult to a stored tool message.
func shrinkStoredToolMessage(contentJSON string) string {
	var msg provider.Message
	if json.Unmarshal([]byte(contentJSON), &msg) != nil {
		return contentJSON
	}
	capped := capToolResult("", msg.Content)
	if capped == msg.Content {
		return contentJSON
	}
	msg.Content = capped
	return store.JSON(msg)
}

// largestContext is the largest context window among available models.
func largestContext(state router.RoutingState) int {
	largest := 0
	for _, id := range state.AvailableModels {
		if m, ok := model.Get(id); ok {
			largest = max(largest, m.ContextWindow)
		}
	}
	return largest
}

// recoverOversizedHistory makes a history that no model can hold fit again:
// it first bounds oversized tool results already stored (which compaction
// keeps when they are recent), then compacts. It reports whether anything
// changed, so the caller can route again.
func (e *Engine) recoverOversizedHistory(ctx context.Context, sid string, inputTokens int, emit EmitFunc) bool {
	shrunk, err := e.store.ShrinkMessages(ctx, sid, "tool", maxToolResultChars, shrinkStoredToolMessage)
	e.emit(ctx, sid, "context.oversized", map[string]any{"input_tokens": inputTokens, "tool_results_shrunk": shrunk, "error": errString(err)}, emit)
	stored, _ := e.store.Messages(ctx, sid)
	s, _ := e.store.Session(ctx, sid)
	if estimate(s.Spec+s.DurableSummary+messagesText(stored)) < inputTokens*3/4 {
		return shrunk > 0
	}
	e.markCompacted(sid)
	e.compact(ctx, sid, emit)
	return true
}

func oversizedHistoryError(inputTokens, largest int) error {
	return fmt.Errorf("history of about %d tokens exceeds the largest available context window (%d tokens) even after shrinking tool results and compacting", inputTokens, largest)
}

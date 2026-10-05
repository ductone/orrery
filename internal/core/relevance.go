package core

import (
	"context"
	"strings"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/store"
)

var relevanceQuestion = map[string]jev.Question{"needed": jev.Noul(
	"Will the agent need this result's content, not just that it ran, to finish the current objective?",
	"The content itself is needed.",
	"A stub that it ran is enough.",
)}

func relevanceKeeps(ctx context.Context, client *jev.Client, objective, tool, args, content string) bool {
	if client == nil {
		return false
	}
	state := map[string]any{"objective": truncate(objective, 2_000), "tool": tool, "arguments": truncate(args, 1_000), "content": truncate(content, 4_000)}
	resp, err := client.Ask(ctx, state, relevanceQuestion)
	if err != nil || resp.Answers["needed"].Noul == nil {
		return false
	}
	return *resp.Answers["needed"].Noul >= 0.7
}

func lostFact(ctx context.Context, client *jev.Client, state DurableState, fact string) bool {
	if client == nil || strings.TrimSpace(fact) == "" {
		return false
	}
	resp, err := client.Ask(ctx, map[string]any{"summary": truncate(store.JSON(state), 24_000), "fact": fact}, map[string]jev.Question{"preserved": jev.Noul("Does the summary preserve this fact?", "It preserves it.", "It drops it.")})
	return err == nil && resp.Answers["preserved"].Noul != nil && *resp.Answers["preserved"].Noul < 0.3
}

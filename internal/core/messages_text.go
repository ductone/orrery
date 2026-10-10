package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

// instructionTextChars bounds the request text sent to a classifier.
const instructionTextChars = 8_000

// waitBackground gives in-flight shadow calls one timeout to land before the
// store closes, so a headless run does not lose its last observations.
func (e *Engine) waitBackground() {
	done := make(chan struct{})
	go func() { e.backgroundWG.Wait(); close(done) }()
	cfg, _, _, _, _ := e.runtimeSnapshot()
	select {
	case <-done:
	case <-time.After(cfg.Jev.Timeout() + time.Second):
	}
}

func (e *Engine) currentTurnID(ctx context.Context, sid string) string {
	if id := turnIDFromContext(ctx); id != "" {
		return id
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turnIDs[sid]
}

// lastUserText returns the latest message a person sent, skipping messages
// the harness wrote.
func lastUserText(stored []store.Message) string {
	for i := len(stored) - 1; i >= 0; i-- {
		if stored[i].Role != "user" {
			continue
		}
		var msg provider.Message
		if json.Unmarshal([]byte(stored[i].ContentJSON), &msg) == nil && !msg.Harness {
			return msg.Content
		}
	}
	return ""
}

// lastAssistantText returns the agent's latest final answer, skipping tool
// calls and harness messages.
func lastAssistantText(stored []store.Message) string {
	for i := len(stored) - 1; i >= 0; i-- {
		if stored[i].Role != "assistant" {
			continue
		}
		var msg provider.Message
		if json.Unmarshal([]byte(stored[i].ContentJSON), &msg) != nil || msg.Harness || len(msg.ToolCalls) > 0 || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		return msg.Content
	}
	return ""
}

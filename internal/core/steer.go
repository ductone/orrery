package core

import (
	"context"

	"github.com/ductone/orrery/internal/provider"
)

// A person can add to a request while its session is working on it. The
// message cannot be written into history mid-turn (it would split a tool
// call from its result), so it waits for the run's next turn boundary, as
// worker handoffs do.

// Steer queues content, a person's message, for session id's run in
// flight: it is added to the transcript as a user message at the run's next
// turn boundary, and the turn after reads it. It reports false, queuing
// nothing, when id has no run in flight; continue the session then. A
// message queued after the run's last boundary is not seen by that run:
// Unsteered returns it once the run is over.
func (e *Engine) Steer(id, content string) bool {
	if content == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[id] <= 0 {
		return false
	}
	if e.steers == nil {
		e.steers = map[string][]string{}
	}
	e.steers[id] = append(e.steers[id], content)
	return true
}

// Unsteered removes and returns the messages Steer queued for session id
// that no turn boundary delivered. Call it after the run is over, before
// steering the session again.
func (e *Engine) Unsteered(id string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.steers[id]
	delete(e.steers, id)
	return out
}

// drainSteers writes the messages steered into sid at a turn boundary.
func (e *Engine) drainSteers(ctx context.Context, sid string, emit EmitFunc) {
	e.mu.Lock()
	queued := e.steers[sid]
	delete(e.steers, sid)
	e.mu.Unlock()
	for _, content := range queued {
		if err := e.store.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Content: content}); err != nil {
			e.emit(ctx, sid, "provider.error", map[string]any{"error": "deliver steered message: " + err.Error()}, emit)
			continue
		}
		e.emit(ctx, sid, "message.steered", map[string]any{"content": content}, emit)
	}
}

package core

import (
	"context"

	"github.com/ductone/orrey/internal/provider"
)

// A background worker can finish while its parent is mid-turn, between a tool
// call and its result. Writing the handoff into history then breaks the
// call-result pairing providers require (a tool_use without its tool_result
// immediately after), which once failed a session outright. Handoffs to a
// running session are queued and delivered at its next turn boundary.

type handoff struct {
	jobID   string
	message provider.Message
}

// beginRun and endRun track which sessions have a run in flight, workers
// included (their runs are not registered as cancellable root turns).
func (e *Engine) beginRun(sid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running == nil {
		e.running = map[string]int{}
	}
	e.running[sid]++
}

func (e *Engine) endRun(sid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[sid]--; e.running[sid] <= 0 {
		delete(e.running, sid)
	}
}

// deliverHandoff writes a worker's handoff now when its parent is idle, and
// otherwise queues it for the parent's next turn.
func (e *Engine) deliverHandoff(ctx context.Context, sid, jobID string, msg provider.Message) {
	e.mu.Lock()
	if e.running[sid] > 0 {
		if e.pendingHandoffs == nil {
			e.pendingHandoffs = map[string][]handoff{}
		}
		e.pendingHandoffs[sid] = append(e.pendingHandoffs[sid], handoff{jobID, msg})
		e.mu.Unlock()
		return
	}
	delivered := e.deliveredJobs[jobID]
	e.mu.Unlock()
	if !delivered {
		_ = e.store.AddMessage(ctx, sid, "user", msg)
	}
}

// drainHandoffs writes queued handoffs at a turn boundary, skipping workers
// whose result the parent already received through job wait.
func (e *Engine) drainHandoffs(ctx context.Context, sid string) {
	e.mu.Lock()
	queued := e.pendingHandoffs[sid]
	delete(e.pendingHandoffs, sid)
	var deliver []provider.Message
	for _, h := range queued {
		if !e.deliveredJobs[h.jobID] {
			deliver = append(deliver, h.message)
		}
	}
	e.mu.Unlock()
	for _, msg := range deliver {
		_ = e.store.AddMessage(ctx, sid, "user", msg)
	}
}

// markDelivered records that a worker's result reached its parent through
// job wait, so the handoff would only repeat it.
func (e *Engine) markDelivered(jobID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deliveredJobs == nil {
		e.deliveredJobs = map[string]bool{}
	}
	e.deliveredJobs[jobID] = true
}

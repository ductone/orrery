package core

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ductone/orrery/agentproto"
)

func todoArgs(items ...[2]string) map[string]any {
	var list []any
	for _, it := range items {
		list = append(list, map[string]any{"text": it[0], "phase": "implement", "status": it[1]})
	}
	return map[string]any{"items": list}
}

type eventLog struct {
	mu     sync.Mutex
	events []agentproto.AgentEvent
}

func (l *eventLog) emit(ev agentproto.AgentEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

// rejections returns the completion.rejected events for open todo items, and
// how many accepted_open_todos events there were.
func (l *eventLog) todoEvents() (rejected int, acceptedOpen int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.events {
		b, _ := json.Marshal(ev.Data)
		switch ev.Type {
		case "completion.rejected":
			if strings.Contains(string(b), `"open todo items"`) {
				rejected++
			}
		case "completion.accepted_open_todos":
			acceptedOpen++
		}
	}
	return rejected, acceptedOpen
}

func requestText(s *scriptedResponses, n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.requests) {
		return ""
	}
	b, _ := json.Marshal(s.requests[n])
	return string(b)
}

func TestCompletionRefusedWhileTodoPlanOpen(t *testing.T) {
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		switch n {
		case 1:
			return responsesCall("c1", "todo", todoArgs([2]string{"read every issue", "in_progress"}, [2]string{"message the owners", "pending"}, [2]string{"archive old notes", "pending"}))
		case 2:
			return responsesText("done early")
		case 3:
			// dropped closes an item as well as completed does.
			return responsesCall("c2", "todo", todoArgs([2]string{"read every issue", "completed"}, [2]string{"message the owners", "completed"}, [2]string{"archive old notes", "dropped"}))
		default:
			return responsesText("all done")
		}
	}}
	var log eventLog
	r := runScripted(t, s, "shared-write", log.emit)
	if r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	if got := len(s.requests); got != 4 {
		t.Fatalf("requests = %d, want 4", got)
	}
	nudge := requestText(s, 2) // the third request follows the refusal
	for _, want := range []string{"Your todo plan still has open items", "read every issue", "message the owners", "dropped if you decided it is not needed"} {
		if !strings.Contains(nudge, want) {
			t.Errorf("request after the refused answer lacks %q", want)
		}
	}
	if rejected, acceptedOpen := log.todoEvents(); rejected != 1 || acceptedOpen != 0 {
		t.Fatalf("open-todo rejections = %d, accepted with open items = %d; want 1, 0", rejected, acceptedOpen)
	}
	if !strings.Contains(string(mustJSON(r.Result)), "all done") {
		t.Fatalf("accepted result = %v, want the answer given after the plan closed", r.Result)
	}
}

func TestCompletionWithOpenTodosAcceptedAfterTwoRefusals(t *testing.T) {
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n == 1 {
			return responsesCall("c1", "todo", todoArgs([2]string{"read every issue", "pending"}))
		}
		return responsesText("done")
	}}
	var log eventLog
	r := runScripted(t, s, "shared-write", log.emit)
	if r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	if got := len(s.requests); got != 4 {
		t.Fatalf("requests = %d, want 4 (plan, two refused answers, accepted answer)", got)
	}
	if rejected, acceptedOpen := log.todoEvents(); rejected != maxTodoRejections || acceptedOpen != 1 {
		t.Fatalf("open-todo rejections = %d, accepted with open items = %d; want %d, 1", rejected, acceptedOpen, maxTodoRejections)
	}
}

func TestCompletionWithoutTodoPlanUnaffected(t *testing.T) {
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	var log eventLog
	r := runScripted(t, s, "shared-write", log.emit)
	if r.Status != agentproto.Pass || len(s.requests) != 1 {
		t.Fatalf("result = %+v after %d requests, want a pass on the first", r, len(s.requests))
	}
	if rejected, acceptedOpen := log.todoEvents(); rejected != 0 || acceptedOpen != 0 {
		t.Fatalf("open-todo events = %d, %d; want none", rejected, acceptedOpen)
	}
}

func TestWorkerCompletionNotGuardedByTodos(t *testing.T) {
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n == 1 {
			return responsesCall("c1", "todo", todoArgs([2]string{"read every file", "pending"}))
		}
		return responsesText("findings")
	}}
	var log eventLog
	r := runScripted(t, s, "read", log.emit)
	if r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	if rejected, _ := log.todoEvents(); rejected != 0 {
		t.Fatalf("worker open-todo rejections = %d, want 0", rejected)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

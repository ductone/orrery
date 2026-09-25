package tui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/store"
)

type eventLog struct {
	t   *testing.T
	seq int
	at  time.Time
	out []Event
}

func (l *eventLog) add(typ string, data any) {
	l.t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		l.t.Fatal(err)
	}
	l.seq++
	l.at = l.at.Add(time.Second)
	l.out = append(l.out, Event{Seq: l.seq, Type: typ, Data: b, CreatedAt: l.at})
}

func replay(events []Event) (*state, []block) {
	s := &state{}
	var blocks []block
	for _, ev := range events {
		blocks = append(blocks, s.apply(ev)...)
	}
	return s, blocks
}

func kinds(blocks []block) []blockKind {
	out := make([]blockKind, len(blocks))
	for i, b := range blocks {
		out[i] = b.kind
	}
	return out
}

func TestReplayBuildsTranscriptAndLiveState(t *testing.T) {
	l := &eventLog{t: t, at: time.Unix(1000, 0)}
	l.add("session.created", map[string]any{})
	l.add("session.started", map[string]any{"spec": "fix the bug"})
	l.add("routing.decision", map[string]any{"decision": map[string]any{"model": map[string]any{"ID": "anthropic/claude-sonnet-5"}, "effort": "medium"}, "explanation": "selected"})
	l.add("tool.started", map[string]any{"id": "c1", "name": "exec", "arguments": map[string]any{"command": "go test ./..."}})
	l.add("message.queued", map[string]any{"request_id": "r1", "content": "also lint"})
	l.add("tool.finished", map[string]any{"call": map[string]any{"id": "c1", "name": "exec"}, "result": map[string]any{"ok": false, "error": "exit 1", "summary": "FAIL"}})
	l.add("assistant.message", map[string]any{"message": map[string]any{"content": `{"summary":"done","changes":["a"]}`}, "model": "anthropic/claude-sonnet-5"})
	l.add("session.terminal", map[string]any{"status": "pass", "result": map[string]any{"summary": "done", "changes": []string{"a"}}})

	s, blocks := replay(l.out)
	want := []blockKind{blockUser, blockRouting, blockTool, blockAssistant, blockTerminal}
	if got := kinds(blocks); len(got) != len(want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("blocks = %v, want %v", got, want)
			}
		}
	}
	if blocks[0].text != "fix the bug" {
		t.Fatalf("initial spec should render as the user prompt, got %q", blocks[0].text)
	}
	tool := blocks[2].tool
	if !tool.failed || tool.elapsed != 2*time.Second {
		t.Fatalf("tool = %+v, want failed with 2s elapsed", tool)
	}
	if blocks[3].structured == nil {
		t.Fatal("a JSON final answer should render as a structured completion")
	}
	if blocks[4].structured != nil {
		t.Fatal("the terminal block must not repeat a structured completion already shown")
	}
	if s.status != "pass" || s.tool != nil || s.model != "anthropic/claude-sonnet-5" {
		t.Fatalf("state = status %q tool %v model %q", s.status, s.tool, s.model)
	}
	if len(s.queued) != 1 || s.queued[0].content != "also lint" {
		t.Fatalf("undelivered queued message should stay pending, got %+v", s.queued)
	}

	// Delivery of the queued message commits it and clears it from the queue.
	l.add("turn.accepted", map[string]any{"request_id": "r1"})
	l.add("user.message", map[string]any{"request_id": "r1", "content": map[string]any{"role": "user", "content": "also lint"}})
	l.add("session.started", map[string]any{"spec": "fix the bug"})
	more := []block{}
	for _, ev := range l.out[len(l.out)-3:] {
		more = append(more, s.apply(ev)...)
	}
	if len(more) != 1 || more[0].kind != blockUser || more[0].text != "also lint" {
		t.Fatalf("delivery blocks = %+v, want only the delivered user message", more)
	}
	if len(s.queued) != 0 || !s.running() {
		t.Fatalf("after delivery queued=%v running=%v", s.queued, s.running())
	}
}

func TestReplayIsIdempotentAndOrdered(t *testing.T) {
	l := &eventLog{t: t, at: time.Unix(1000, 0)}
	l.add("session.started", map[string]any{"spec": "task"})
	l.add("input.required", map[string]any{"id": "q1", "question": "which db?", "choices": []string{"pg", "sqlite"}})
	s, blocks := replay(l.out)
	if s.status != "input_required" || s.input == nil || len(s.input.Choices) != 2 {
		t.Fatalf("pending input not tracked: %+v", s)
	}
	if again := s.apply(l.out[1]); again != nil {
		t.Fatalf("re-applying a delivered event must be a no-op, got %v", again)
	}
	l.add("input.answered", map[string]any{"id": "q1"})
	s.apply(l.out[2])
	if s.input != nil {
		t.Fatal("answered input should clear")
	}
	if len(blocks) != 2 || blocks[1].kind != blockInput {
		t.Fatalf("blocks = %v", kinds(blocks))
	}
}

func TestIdleSessionFirstMessageIsNotDuplicated(t *testing.T) {
	// ACP/idle sessions carry a placeholder spec; the real prompt arrives as
	// a user.message before the first session.started.
	l := &eventLog{t: t, at: time.Unix(1000, 0)}
	l.add("user.message", map[string]any{"request_id": "r", "content": map[string]any{"role": "user", "content": "real task"}})
	l.add("session.started", map[string]any{"spec": "Interactive coding session"})
	_, blocks := replay(l.out)
	if len(blocks) != 1 || blocks[0].text != "real task" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestReconcileOnlyOverridesWithNewerSnapshots(t *testing.T) {
	l := &eventLog{t: t, at: time.Unix(1000, 0)}
	l.add("session.started", map[string]any{"spec": "task"})
	s, _ := replay(l.out)

	s.reconcile(store.Session{ID: "s", Status: "pass", UpdatedAt: l.at.Add(-time.Second)})
	if !s.running() {
		t.Fatal("a snapshot older than the newest event must not end a running turn")
	}
	s.reconcile(store.Session{ID: "s", Status: "interrupted", UpdatedAt: l.at.Add(time.Second)})
	if s.status != "interrupted" {
		t.Fatalf("a newer snapshot should end a turn that stopped without a terminal event, got %q", s.status)
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in        string
		name      string
		arg       string
		isCommand bool
	}{
		{"/help", "help", "", true},
		{"/budget 5", "budget", "5", true},
		{"/tmp/x is broken", "", "", false},
		{"//help", "", "", false},
		{"/.hidden", "", "", false},
		{"hello", "", "", false},
	}
	for _, c := range cases {
		name, arg, ok := parseCommand(c.in)
		if ok != c.isCommand || name != c.name || arg != c.arg {
			t.Errorf("parseCommand(%q) = %q %q %v", c.in, name, arg, ok)
		}
	}
}

func TestSelectCheckpoint(t *testing.T) {
	cps := []store.Checkpoint{{ID: "abc123", Label: "one"}, {ID: "abd456", Label: "two"}}
	if cp, err := selectCheckpoint(cps, "#2"); err != nil || cp.ID != "abd456" {
		t.Fatalf("index selection = %v %v", cp, err)
	}
	if cp, err := selectCheckpoint(cps, "abc"); err != nil || cp.ID != "abc123" {
		t.Fatalf("prefix selection = %v %v", cp, err)
	}
	if _, err := selectCheckpoint(cps, "ab"); err == nil {
		t.Fatal("ambiguous prefix must fail")
	}
}

func TestFuzzyPrefersBasenameMatches(t *testing.T) {
	files := []string{"internal/model/catalog.go", "cmd/orrery/main.go", "internal/tui/model.go"}
	got := fuzzyFilter("model", files, 3)
	if len(got) == 0 || got[0] != "internal/tui/model.go" {
		t.Fatalf("fuzzyFilter = %v", got)
	}
	if got := fuzzyFilter("zzz", files, 3); len(got) != 0 {
		t.Fatalf("non-matching pattern returned %v", got)
	}
}

func TestSnapshotBeforeReplayDoesNotDoubleCount(t *testing.T) {
	l := &eventLog{t: t, at: time.Unix(1000, 0)}
	l.add("usage.reported", map[string]any{"model": "m", "cost_usd": 0.5})
	s := &state{}
	s.reconcile(store.Session{ID: "s", Status: "pass", SpentUSD: 0.5, UpdatedAt: l.at})
	s.apply(l.out[0])
	if got := s.spend(); got != 0.5 {
		t.Fatalf("spend = %v, want 0.5", got)
	}
	s.reconcile(store.Session{ID: "s", Status: "pass", SpentUSD: 0.75, UpdatedAt: l.at})
	if got := s.spend(); got != 0.75 {
		t.Fatalf("ledger spend from workers should win, got %v", got)
	}
}

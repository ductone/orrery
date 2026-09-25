package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ductone/orrey/internal/store"
)

func TestSquireContract(t *testing.T) {
	dir, err := os.MkdirTemp("", "sq")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var mu sync.Mutex
	var got []string
	sq, err := StartSquire(dir, "task-1", func(text string) error {
		mu.Lock()
		defer mu.Unlock()
		if text == "fail" {
			return errors.New("rejected")
		}
		got = append(got, text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sq.SocketPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info.Mode(), err)
	}

	conn, err := net.Dial("unix", sq.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`{"id":"1","type":"prompt","message":"hello\nworld"}`,
		`{"id":"2","type":"steer","message":"x"}`,
		`{"id":"3","type":"prompt","message":""}`,
		`{"id":"4","type":"prompt","message":"fail"}`,
		`not json`,
	} {
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	r := bufio.NewReader(conn)
	var responses []controlResponse
	for range 5 {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var res controlResponse
		if err := json.Unmarshal(line, &res); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, res)
	}
	conn.Close()
	if !responses[0].Success || responses[0].ID != "1" || responses[0].Command != "prompt" {
		t.Fatalf("prompt response = %+v", responses[0])
	}
	for i, want := range []string{"unknown command steer", "message is required", "rejected", "malformed JSON"} {
		if res := responses[i+1]; res.Success || res.Error != want {
			t.Fatalf("response %d = %+v, want error %q", i+1, res, want)
		}
	}
	mu.Lock()
	if len(got) != 1 || got[0] != "hello\nworld" {
		t.Fatalf("submitted = %q", got)
	}
	mu.Unlock()

	if err := sq.Bind("session-1"); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(dir, "pids", strconv.Itoa(os.Getpid())+".json")
	var binding map[string]any
	b, err := os.ReadFile(pidPath)
	if err != nil || json.Unmarshal(b, &binding) != nil {
		t.Fatalf("pid sidecar: %s %v", b, err)
	}
	if binding["current_session"] != "session-1" || int(binding["pid"].(float64)) != os.Getpid() || binding["started_at"] == "" {
		t.Fatalf("binding = %v", binding)
	}
	sq.Record([]Event{{Seq: 1, Type: "session.started", Data: json.RawMessage(`{}`)}, {Seq: 2, Type: "usage.reported", Data: json.RawMessage(`{"cost_usd":0.1}`)}})
	journal, err := os.ReadFile(filepath.Join(dir, "sessions", "session-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(journal)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"type":"usage.reported"`) {
		t.Fatalf("journal = %q", journal)
	}

	if err := sq.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sq.SocketPath(), pidPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed on close: %v", p, err)
		}
	}
}

// fakeBackend records calls; Create blocks until release is closed so tests
// can submit more messages while the session is being created.
type fakeBackend struct {
	mu      sync.Mutex
	release chan struct{}
	creates []CreateRequest
	sends   []string
}

func (f *fakeBackend) Describe() string { return "fake" }
func (f *fakeBackend) Session(context.Context, string) (store.Session, error) {
	return store.Session{ID: "s1", Status: "running"}, nil
}
func (f *fakeBackend) Lookup(context.Context, string, string, string) (store.Session, error) {
	return store.Session{}, ErrNotFound
}
func (f *fakeBackend) Create(_ context.Context, req CreateRequest) (string, error) {
	<-f.release
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	return "s1", nil
}
func (f *fakeBackend) Send(_ context.Context, id, content, _ string) (SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, id+":"+content)
	return SendResult{Queued: true}, nil
}
func (f *fakeBackend) Stream(ctx context.Context, _ string, _ int, _ chan<- []Event) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeBackend) Cancel(context.Context, string) (bool, error) { return true, nil }
func (f *fakeBackend) Compact(context.Context, string) error        { return nil }
func (f *fakeBackend) Checkpoint(context.Context, string, string) (store.Checkpoint, error) {
	return store.Checkpoint{}, nil
}
func (f *fakeBackend) Checkpoints(context.Context, string) ([]store.Checkpoint, error) {
	return nil, nil
}
func (f *fakeBackend) Restore(context.Context, string, string) error { return nil }
func (f *fakeBackend) AddBudget(context.Context, string, float64) (bool, error) {
	return false, nil
}

// drive runs cmd and feeds the resulting messages back into the model,
// following batches, until only deliveries of interest remain.
func drive(t *testing.T, m *model, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > 8 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(100 * time.Millisecond):
		return // timers, streams, and other long-lived commands
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			drive(t, m, c, depth+1)
		}
		return
	}
	switch msg.(type) {
	case deliveredMsg, actionMsg:
		_, next := m.Update(msg)
		drive(t, m, next, depth+1)
	}
}

func TestFirstMessageCreatesSessionAndLaterMessagesWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBackend{release: make(chan struct{})}
	m := newModel(ctx, Options{Backend: fb, Create: CreateRequest{Workspace: "/w", Integration: "squire", ExternalID: "task"}}, "")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	createCmd := m.submit("first task")
	if !m.creating {
		t.Fatal("first message in a draft session should start creation")
	}
	if cmd := m.submit("second"); cmd != nil {
		t.Fatal("messages submitted during creation must wait in the outbox")
	}
	reply := make(chan error, 1)
	m.Update(controlMsg{text: "from squire", reply: reply})

	close(fb.release)
	drive(t, m, createCmd, 0)

	if m.sessionID != "s1" || m.creating {
		t.Fatalf("session = %q creating = %v", m.sessionID, m.creating)
	}
	if len(fb.creates) != 1 || fb.creates[0].Prompt != "first task" || fb.creates[0].ExternalID != "task" || fb.creates[0].Workspace != "/w" {
		t.Fatalf("creates = %+v", fb.creates)
	}
	fb.mu.Lock()
	sends := append([]string(nil), fb.sends...)
	fb.mu.Unlock()
	if len(sends) != 2 || sends[0] != "s1:second" || sends[1] != "s1:from squire" {
		t.Fatalf("sends = %v, want outbox flushed in submission order", sends)
	}
	select {
	case err := <-reply:
		if err != nil {
			t.Fatalf("control prompt reply = %v", err)
		}
	default:
		t.Fatal("control prompt was never acknowledged")
	}
}

func TestInputChoicesRejectFreeformText(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBackend{release: make(chan struct{})}
	m := newModel(ctx, Options{Backend: fb}, "s1")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	b, _ := json.Marshal(map[string]any{"id": "q", "question": "db?", "choices": []string{"pg", "sqlite"}})
	m.onEvents([]Event{{Seq: 1, Type: "input.required", Data: b, CreatedAt: time.Now()}})

	m.submit("mysql")
	fb.mu.Lock()
	n := len(fb.sends)
	fb.mu.Unlock()
	if n != 0 {
		t.Fatal("an answer outside the offered choices must not be sent")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	drive(t, m, m.onKey(tea.KeyPressMsg{Code: tea.KeyEnter}), 0)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.sends) != 1 || fb.sends[0] != "s1:sqlite" {
		t.Fatalf("sends = %v, want the selected choice", fb.sends)
	}
}

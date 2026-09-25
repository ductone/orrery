package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/ductone/orrey/internal/web"
)

const remoteTestCompletion = `{"model":"gpt-5.6-terra","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":10,"output_tokens":5}}`

// remoteTestServer serves a real web.Server over a real engine whose model
// provider answers once gate is closed.
func remoteTestServer(t *testing.T, gate <-chan struct{}) (*Remote, *core.Engine, *store.Store) {
	t.Helper()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(remoteTestCompletion))
	}))
	t.Cleanup(llm.Close)
	st, err := store.Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{
		WorkspaceRoot: t.TempDir(),
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: llm.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
	e := core.New(cfg, st, provider.New(cfg), nil)
	srv := httptest.NewServer(web.New("127.0.0.1:0", e, "test").Handler())
	t.Cleanup(srv.Close)
	r, err := NewRemote(srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r, e, st
}

func waitIdle(t *testing.T, e *core.Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for quiet := 0; quiet < 3; time.Sleep(50 * time.Millisecond) {
		if len(e.ActiveTurns()) > 0 {
			quiet = 0
		} else {
			quiet++
		}
		if time.Now().After(deadline) {
			t.Fatalf("turns still active: %v", e.ActiveTurns())
		}
	}
}

func TestRemoteNewValidatesURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://host", "localhost:8080", "http://", "http://host/?x=1"} {
		if _, err := NewRemote(bad, nil); err == nil {
			t.Errorf("NewRemote(%q) accepted", bad)
		}
	}
	r, err := NewRemote("https://orrery.example/prefix/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Describe() != "https://orrery.example/prefix" {
		t.Fatalf("Describe() = %q", r.Describe())
	}
}

func TestRemoteCreateLookupSend(t *testing.T) {
	gate := make(chan struct{})
	r, e, st := remoteTestServer(t, gate)
	ctx := context.Background()
	ws := t.TempDir()

	if _, err := r.Lookup(ctx, "squire", "task-1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup before create: %v", err)
	}
	if _, err := r.Create(ctx, CreateRequest{Prompt: "hi", Workspace: ws}); err == nil || !strings.Contains(err.Error(), "--external-id") {
		t.Fatalf("create without identity: %v", err)
	}
	req := CreateRequest{Prompt: "do the thing", Workspace: ws, BudgetUSD: 2, Integration: "squire", ExternalID: "task-1"}
	id, err := r.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, CreateRequest{Prompt: "pinned", Workspace: ws, TierPin: "frontier", ExternalIncarnation: "7", Integration: "squire", ExternalID: "task-2"}); err != nil {
		t.Fatalf("create with routing and incarnation: %v", err)
	}
	if again, err := r.Create(ctx, req); err != nil || again != id {
		t.Fatalf("create is not idempotent on external identity: %q %v (want %q)", again, err, id)
	}
	found, err := r.Lookup(ctx, "squire", "task-1", "")
	if err != nil || found.ID != id || found.WorkspacePath != ws || found.BudgetUSD != 2 {
		t.Fatalf("lookup = %+v, %v", found, err)
	}
	if _, err := r.Lookup(ctx, "squire", "task-1", "2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other incarnation must not match: %v", err)
	}
	if s, err := r.Session(ctx, id); err != nil || s.ExternalID != "task-1" {
		t.Fatalf("session = %+v, %v", s, err)
	}
	if _, err := r.Session(ctx, "no/such session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}

	// The first turn is parked on the gate, so a message queues.
	if res, err := r.Send(ctx, id, "follow up", "req-1"); err != nil || !res.Queued || res.Duplicate {
		t.Fatalf("send while running = %+v, %v", res, err)
	}
	if res, err := r.Send(ctx, id, "follow up", "req-1"); err != nil || !res.Duplicate {
		t.Fatalf("resend = %+v, %v", res, err)
	}
	if _, err := r.Send(ctx, id, "different", "req-1"); err == nil {
		t.Fatal("reused request id with a new payload must fail")
	}
	close(gate)
	waitIdle(t, e)

	if err := st.CreateSession(ctx, store.Session{ID: "idle", Spec: "x", Status: "pass", BudgetUSD: 1, WorkspacePath: ws}); err != nil {
		t.Fatal(err)
	}
	if res, err := r.Send(ctx, "idle", "go", "req-2"); err != nil || res.Queued || res.Duplicate {
		t.Fatalf("send while idle = %+v, %v", res, err)
	}
	waitIdle(t, e)
}

func TestRemoteSessionOperations(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	r, _, st := remoteTestServer(t, gate)
	ctx := context.Background()
	if err := st.CreateSession(ctx, store.Session{ID: "s", Spec: "x", Status: "pass", BudgetUSD: 3}); err != nil {
		t.Fatal(err)
	}
	if xs, err := r.Checkpoints(ctx, "s"); err != nil || xs == nil || len(xs) != 0 {
		t.Fatalf("checkpoints = %#v, %v", xs, err)
	}
	cp, err := r.Checkpoint(ctx, "s", "before")
	if err != nil || cp.ID == "" || cp.Label != "before" {
		t.Fatalf("checkpoint = %+v, %v", cp, err)
	}
	if xs, err := r.Checkpoints(ctx, "s"); err != nil || len(xs) != 1 || xs[0].ID != cp.ID {
		t.Fatalf("checkpoints = %+v, %v", xs, err)
	}
	if err := r.Restore(ctx, "s", cp.ID); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := r.Cancel(ctx, "s"); err != nil || cancelled {
		t.Fatalf("cancel idle = %v, %v", cancelled, err)
	}
	if resumed, err := r.AddBudget(ctx, "s", 2); err != nil || resumed {
		t.Fatalf("add budget = %v, %v", resumed, err)
	}
	if s, _ := st.Session(ctx, "s"); s.BudgetUSD != 5 {
		t.Fatalf("budget = %v", s.BudgetUSD)
	}
}

func TestRemoteErrorBodiesWithStatus200(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	r, _, st := remoteTestServer(t, gate)
	ctx := context.Background()
	if err := st.CreateSession(ctx, store.Session{ID: "s", Spec: "x", Status: "pass", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	// These failures come back as 200 with a plain-text body.
	var re *remoteError
	if err := r.Restore(ctx, "s", "missing-checkpoint"); !errors.As(err, &re) || re.status != http.StatusOK || re.message == "" || strings.Contains(re.message, "\n") {
		t.Fatalf("restore missing checkpoint: %v", err)
	}
	if _, err := r.Checkpoint(ctx, "missing", "x"); err == nil {
		t.Fatal("checkpoint of missing session succeeded")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("  compaction exploded\n"))
	}))
	defer srv.Close()
	fake, _ := NewRemote(srv.URL, nil)
	if err := fake.Compact(ctx, "s"); err == nil || err.Error() != "compaction exploded" {
		t.Fatalf("compact = %v", err)
	}
	if _, err := fake.Cancel(ctx, "s"); err == nil || err.Error() != "compaction exploded" {
		t.Fatalf("cancel = %v", err)
	}
}

func TestRemoteStreamFromServer(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	r, _, st := remoteTestServer(t, gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.CreateSession(ctx, store.Session{ID: "s", Spec: "x", Status: "pass", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		_, _ = st.AddEvent(ctx, "s", "note", map[string]int{"n": i})
	}
	out := make(chan []Event)
	done := make(chan error, 1)
	go func() { done <- r.Stream(ctx, "s", 1, out) }()
	got := collect(t, out, 2)
	_, _ = st.AddEvent(ctx, "s", "note", map[string]int{"n": 4})
	got = append(got, collect(t, out, 1)...)
	if seqs(got) != "2,3,4" || string(got[2].Data) != `{"n":4}` || got[0].SessionID != "s" {
		t.Fatalf("events = %s %s", seqs(got), got[2].Data)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop on cancel")
	}
}

func TestRemoteStreamResumesWithoutDuplicates(t *testing.T) {
	frame := func(seq int) string {
		return fmt.Sprintf("id: s:%d\nevent: note\ndata: {\"sequence\":%d,\n: comment\ndata: \"session_id\":\"s\",\"type\":\"note\",\"data\":{\"n\":%d}}\n\n", seq, seq, seq)
	}
	var (
		mu     sync.Mutex
		afters []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after"))
		n := len(afters)
		mu.Unlock()
		if n == 1 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		if n == 2 {
			// Deliver three frames in one write, then drop the connection.
			_, _ = w.Write([]byte(frame(1) + frame(2) + frame(3)))
			return
		}
		// Replay one already-delivered frame before the new ones.
		for seq := after; seq <= 5; seq++ {
			_, _ = w.Write([]byte(frame(seq)))
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	r, _ := NewRemote(srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan []Event)
	done := make(chan error, 1)
	go func() { done <- r.Stream(ctx, "s", 0, out) }()
	got := collect(t, out, 5)
	if seqs(got) != "1,2,3,4,5" || string(got[4].Data) != `{"n":5}` || got[4].Type != "note" {
		t.Fatalf("events = %s", seqs(got))
	}
	mu.Lock()
	if strings.Join(afters, ",") != "0,0,3" {
		t.Fatalf("reconnect cursors = %v", afters)
	}
	mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop on cancel")
	}
}

// Routing decisions carry full candidate lists, so real frames regularly
// exceed the reader's buffer; they must arrive intact.
func TestRemoteStreamLargeFrames(t *testing.T) {
	pad := strings.Repeat("x", 9000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for seq := 1; seq <= 3; seq++ {
			fmt.Fprintf(w, "id: s:%d\nevent: big\ndata: {\"sequence\":%d,\"type\":\"big\",\"data\":{\"pad\":%q}}\n\n", seq, seq, pad)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	r, _ := NewRemote(srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan []Event)
	go func() { _ = r.Stream(ctx, "s", 0, out) }()
	got := collect(t, out, 3)
	if seqs(got) != "1,2,3" {
		t.Fatalf("events = %s", seqs(got))
	}
	for _, ev := range got {
		if string(ev.Data) != fmt.Sprintf(`{"pad":%q}`, pad) {
			t.Fatalf("event %d data corrupted (%d bytes)", ev.Seq, len(ev.Data))
		}
	}
}

func TestRemoteStreamCancelWhileBlockedOnSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"sequence\":1}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	r, _ := NewRemote(srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Stream(ctx, "s", 0, make(chan []Event)) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream blocked on an unread channel after cancel")
	}
}

func collect(t *testing.T, out <-chan []Event, n int) []Event {
	t.Helper()
	var got []Event
	timeout := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case batch := <-out:
			if len(batch) == 0 {
				t.Fatal("empty batch")
			}
			got = append(got, batch...)
		case <-timeout:
			t.Fatalf("got %d of %d events: %s", len(got), n, seqs(got))
		}
	}
	return got
}

func seqs(xs []Event) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x.Seq)
	}
	return strings.Join(out, ",")
}

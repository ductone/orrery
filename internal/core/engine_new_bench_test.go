package core

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

// benchStore opens a SQLite store holding n finished top-level sessions whose
// memory extraction already ran: the state of an agent's store when it wakes.
func benchStore(tb testing.TB, n int) *store.Store {
	tb.Helper()
	st, err := store.Open(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for i := range n {
		id := fmt.Sprintf("bench-%04d", i)
		if err := st.CreateSession(ctx, store.Session{ID: id, Spec: "task " + id, BudgetUSD: 1}); err != nil {
			tb.Fatal(err)
		}
		if _, err := st.AddEvent(ctx, id, "session.started", map[string]any{"spec": "task"}); err != nil {
			tb.Fatal(err)
		}
		ev, err := st.AddEvent(ctx, id, "session.terminal", map[string]any{"status": "pass"})
		if err != nil {
			tb.Fatal(err)
		}
		if err := st.AdvanceMemoryWatermark(ctx, id, ev.Seq); err != nil {
			tb.Fatal(err)
		}
		if err := st.SetSessionStatus(ctx, id, "pass"); err != nil {
			tb.Fatal(err)
		}
	}
	return st
}

// BenchmarkEngineNewClose measures what waking an agent costs: constructing
// an engine over a store with 200 finished sessions, and closing it.
func BenchmarkEngineNewClose(b *testing.B) {
	for _, tc := range []struct {
		name      string
		providers *provider.Registry
	}{
		{"no-providers", nil},
		{"providers", provider.New(config.Config{})},
	} {
		b.Run(tc.name, func(b *testing.B) {
			st := benchStore(b, 200)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				e := New(config.Config{}, st, tc.providers, nil)
				if err := e.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestEngineNewCloseReleasesGoroutines checks an engine leaves no goroutines
// behind once closed, since a host creates and closes one per agent wake.
func TestEngineNewCloseReleasesGoroutines(t *testing.T) {
	st := benchStore(t, 20)
	for _, p := range []*provider.Registry{nil, provider.New(config.Config{})} {
		before := runtime.NumGoroutine()
		for range 5 {
			e := New(config.Config{}, st, p, nil, WithScratchRoot(t.TempDir()))
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines: %d before, %d after New+Close (providers=%v)", before, after, p != nil)
		}
	}
}

// TestCatchUpMemorySchedulesOnlyPending checks wake-up schedules extraction
// for sessions with a passing terminal beyond the watermark, not every
// finished session.
func TestCatchUpMemorySchedulesOnlyPending(t *testing.T) {
	st := benchStore(t, 5)
	ctx := context.Background()
	// A sixth session finished passing but was never extracted.
	if err := st.CreateSession(ctx, store.Session{ID: "pending", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEvent(ctx, "pending", "session.terminal", map[string]any{"status": "pass"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionStatus(ctx, "pending", "pass"); err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: st, reads: map[string]int{}}
	e := New(config.Config{}, cs, provider.New(config.Config{}), nil, WithScratchRoot(t.TempDir()))
	e.waitBackground()
	_ = e.Close()
	// memoryPending reads a session's events once; extraction reads them again.
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for id, n := range cs.reads {
		want := 1
		if id == "pending" {
			want = 2
		}
		if n != want {
			t.Errorf("session %s: %d event reads, want %d", id, n, want)
		}
	}
	if len(cs.reads) != 6 {
		t.Errorf("scanned %d sessions, want 6", len(cs.reads))
	}
}

// countingStore counts EventsAfter reads per session.
type countingStore struct {
	*store.Store
	mu    sync.Mutex
	reads map[string]int
}

func (c *countingStore) EventsAfter(ctx context.Context, sid string, after int) ([]storage.Event, error) {
	c.mu.Lock()
	c.reads[sid]++
	c.mu.Unlock()
	return c.Store.EventsAfter(ctx, sid, after)
}

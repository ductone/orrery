package core

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
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

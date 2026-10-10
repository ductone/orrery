package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestModelStatsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Failures before the first success must not dilute the initial EWMA.
	for _, kind := range []string{"empty", "malformed", "provider_error"} {
		if err := s.RecordModelFailure(ctx, "ramp/test", kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordModelCall(ctx, "ramp/test", 200*time.Second, 1000, true, 5000, 4000); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordModelCall(ctx, "ramp/test", 10*time.Second, 200, false, 1000, 250); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ModelStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	x := stats[0]
	if x.Route != "ramp/test" || x.Calls != 2 || x.Truncated != 1 || x.Empty != 1 || x.Malformed != 1 || x.ProviderErrors != 1 || x.InputTokens != 6000 || x.CacheReadTokens != 4250 || math.Abs(x.LatencySeconds-181) > 1e-9 || math.Abs(x.OutputTokensPerSecond-6.5) > 1e-9 {
		t.Fatalf("stats=%+v", x)
	}
	if x.LastCall.IsZero() || x.LastSlowCall.IsZero() || x.UpdatedAt.Before(x.LastCall) || x.LastCall.Before(x.LastSlowCall) {
		t.Fatalf("timestamps=%+v", x)
	}
	if err := s.RecordModelFailure(ctx, "ramp/test", "unknown"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestModelStatsBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, Session{ID: "a", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	events := []struct {
		kind string
		data any
	}{
		{"routing.selected", map[string]any{"decision": map[string]any{"model": map[string]any{"id": "ramp/test"}}}},
		{"provider.error", map[string]any{"model": "ramp/test", "error": "bad"}},
		{"completion.rejected", map[string]any{"reason": "malformed tool-call arguments"}},
		{"usage.reported", map[string]any{"model": "ramp/test", "latency": 200 * time.Second, "output_tokens": 1000, "truncated": true}},
		{"usage.reported", map[string]any{"model": "ramp/test", "latency": 10 * time.Second, "output_tokens": 200}},
		{"completion.rejected", map[string]any{"reason": "empty assistant response"}},
		{"usage.reported", map[string]any{"model": "ramp/test", "kind": "compaction", "output_tokens": 100}},
		{"provider.error", map[string]any{"error": "store failure, not a provider call"}},
	}
	for _, ev := range events {
		if _, err := s.AddEvent(ctx, "a", ev.kind, ev.data); err != nil {
			t.Fatal(err)
		}
	}
	// Opening an empty stats table replays historical events exactly once.
	s.Close()
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := s.ModelStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(stats) != 1 {
			t.Fatalf("stats=%+v", stats)
		}
		x := stats[0]
		if x.Calls != 2 || x.Truncated != 1 || x.Empty != 1 || x.Malformed != 1 || x.ProviderErrors != 1 || math.Abs(x.LatencySeconds-181) > 1e-9 || math.Abs(x.OutputTokensPerSecond-6.5) > 1e-9 || x.LastSlowCall.IsZero() {
			t.Fatalf("stats=%+v", x)
		}
		s.Close()
	}
}

func TestEffortStatsBackfillFromUsageEvents(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.CreateSession(ctx, Session{ID: "a", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(ctx, "a", "usage.reported", map[string]any{"model": "ramp/x", "effort": "low", "latency": 2 * time.Second, "output_tokens": 100, "input_tokens": 1000}); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillEffortStats(); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ModelStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range stats {
		if st.Route == "ramp/x@low" && st.Calls == 1 && st.LatencySeconds == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("stats = %+v", stats)
	}
}

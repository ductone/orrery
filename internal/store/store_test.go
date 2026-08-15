package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSessionEventsCacheAndRouting(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvent(ctx, "s", "one", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvent(ctx, "s", "two", nil); err != nil {
		t.Fatal(err)
	}
	es, err := s.EventsAfter(ctx, "s", 1)
	if err != nil || len(es) != 1 || es[0].Seq != 2 {
		t.Fatalf("events %+v %v", es, err)
	}
	if err = s.WarmCache(ctx, "s", "m", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, err := s.Cache(ctx, "s", "m")
	if err != nil || !c.Valid(time.Now()) || c.WarmPrefixTokens != 42 {
		t.Fatalf("cache %+v %v", c, err)
	}
	r := RoutingRecord{ID: "r", SessionID: "s", Turn: 1, DecisionPoint: "turn", StateJSON: "{}", CandidatesJSON: "[]", ChosenModel: "m", ChosenEffort: "low", CacheEstJSON: "{}", Explanation: "because"}
	if err = s.WriteRouting(ctx, r); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err = s.ExportRouting(ctx, time.Unix(0, 0), func([]byte) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("export n=%d err=%v", n, err)
	}
}
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.AddEvent(ctx, "s", "event", nil); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.EventsAfter(ctx, "s", 0)
	if err != nil || len(events) != 20 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

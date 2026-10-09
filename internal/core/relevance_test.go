package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

// oldToolHistory returns n old tool results (ids c0..cN-1, "keep" content
// for even ids) followed by 11 recent assistant turns, so all n are eligible
// for masking.
func oldToolHistory(n int) []store.Message {
	var msgs []store.Message
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, store.Message{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: id, Name: "read", Arguments: map[string]any{"path": id}}}})})
		content := "drop"
		if i%2 == 0 {
			content = "keep"
		}
		msgs = append(msgs, store.Message{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: id, Content: content})})
	}
	for i := 0; i < 11; i++ {
		msgs = append(msgs, store.Message{Role: "assistant", ContentJSON: "{}"})
	}
	return msgs
}

type countingAsker struct {
	mu    sync.Mutex
	calls map[string]int
	live  atomic.Int32
	peak  atomic.Int32
	fail  bool
}

func (a *countingAsker) ask(_ context.Context, objective, _, args, content string) (bool, bool) {
	n := a.live.Add(1)
	defer a.live.Add(-1)
	for {
		p := a.peak.Load()
		if n <= p || a.peak.CompareAndSwap(p, n) {
			break
		}
	}
	a.mu.Lock()
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	a.calls[objective+"|"+args]++
	a.mu.Unlock()
	if a.fail {
		return false, false
	}
	return content == "keep", true
}

func (a *countingAsker) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		n += c
	}
	return n
}

func clearedCount(msgs []store.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.ContentJSON, "cleared") {
			n++
		}
	}
	return n
}

func TestMaskingCacheAvoidsRepeatedAsks(t *testing.T) {
	var cache sync.Map
	a := &countingAsker{}
	msgs := oldToolHistory(20)
	if _, stats := maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", msgs); stats.Asked != 20 || stats.Cleared != 10 {
		t.Fatalf("first pass stats = %+v", stats)
	}
	if a.peak.Load() > relevanceConcurrency {
		t.Fatalf("peak concurrency %d exceeds %d", a.peak.Load(), relevanceConcurrency)
	}
	// Kept results stay eligible; a fresh history must be answered from cache.
	fresh := oldToolHistory(20)
	_, stats := maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", fresh)
	if stats.Asked != 0 || stats.Cached != 20 || a.total() != 20 || clearedCount(fresh) != 10 {
		t.Fatalf("second pass stats = %+v asks=%d cleared=%d", stats, a.total(), clearedCount(fresh))
	}
	for k, c := range a.calls {
		if c != 1 {
			t.Fatalf("%s asked %d times", k, c)
		}
	}
}

func TestMaskingCacheInvalidatesOnObjectiveChange(t *testing.T) {
	e, _ := testEngine(t)
	a := &countingAsker{}
	first := e.relevanceCacheFor("s", "obj one")
	maskOldToolResultsCached(t.Context(), a.ask, &first.answers, "obj one", oldToolHistory(4))
	if e.relevanceCacheFor("s", "obj one") != first {
		t.Fatal("same objective must reuse the cache")
	}
	second := e.relevanceCacheFor("s", "obj two")
	if second == first {
		t.Fatal("new objective must replace the cache")
	}
	if _, stats := maskOldToolResultsCached(t.Context(), a.ask, &second.answers, "obj two", oldToolHistory(4)); stats.Asked != 4 || stats.Cached != 0 {
		t.Fatalf("stats after objective change = %+v", stats)
	}
	if a.total() != 8 {
		t.Fatalf("asks = %d, want 8", a.total())
	}
	if e.relevanceCacheFor("other", "obj two") == second {
		t.Fatal("sessions must not share caches")
	}
}

func TestMaskingDoesNotCacheFailedAnswers(t *testing.T) {
	var cache sync.Map
	a := &countingAsker{fail: true}
	maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", oldToolHistory(3))
	maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", oldToolHistory(3))
	if a.total() != 6 {
		t.Fatalf("failed answers must be re-asked, asks = %d", a.total())
	}
}

func TestCachedMaskingKeepsUnansweredResults(t *testing.T) {
	e, _ := testEngine(t)
	var warm sync.Map
	dropC1 := func(_ context.Context, _, _, args, _ string) (bool, bool) { return !strings.Contains(args, "c1"), true }
	maskOldToolResultsCached(t.Context(), dropC1, &warm, "obj", oldToolHistory(2))
	r := e.relevanceCacheFor("s", "obj")
	warm.Range(func(k, v any) bool {
		if !v.(bool) {
			r.answers.Store(k, v)
		}
		return true
	})
	msgs := oldToolHistory(4)
	masked, stats, run := e.applyCachedMasking(t.Context(), "s", "obj", msgs)
	if !masked || stats.Cleared != 1 || stats.Cached != 1 || stats.Asked != 0 || run != nil {
		t.Fatalf("masked=%v stats=%+v run=%v", masked, stats, run)
	}
	if !strings.Contains(msgs[3].ContentJSON, "cleared") || clearedCount(msgs) != 1 {
		t.Fatalf("only the cached clear answer may apply: %v", msgs)
	}
}

func TestScheduledMaskingWarmsCacheForNextTurn(t *testing.T) {
	e, st := testEngine(t)
	ctx := t.Context()
	s := store.Session{ID: "mask-session", Spec: "x", Model: "m", BudgetUSD: 1}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceMessages(ctx, s.ID, oldToolHistory(4)); err != nil {
		t.Fatal(err)
	}
	e.scheduleMasking(ctx, s.ID, "obj")
	e.shadowWG.Wait()
	msgs, err := st.Messages(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	masked, stats, run := e.applyCachedMasking(ctx, s.ID, "obj", msgs)
	if !masked || stats.Cached != 4 || stats.Cleared != 4 || run == nil || run.stats.Asked != 4 {
		t.Fatalf("masked=%v stats=%+v run=%+v", masked, stats, run)
	}
	if _, _, again := e.applyCachedMasking(ctx, s.ID, "obj", msgs); again != nil {
		t.Fatal("a background pass must be reported once")
	}
}

func TestRelevanceExperimentFailsOpenWithoutClient(t *testing.T) {
	if relevanceKeeps(t.Context(), nil, "objective", "read", "{}", "content") || lostFact(t.Context(), nil, DurableState{}, "fact") {
		t.Fatal("disabled Jev relevance must not retain or reject")
	}
}

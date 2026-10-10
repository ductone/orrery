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
	if _, stats := maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", maskGate{}, msgs); stats.Asked != 20 || stats.Cleared != 10 {
		t.Fatalf("first pass stats = %+v", stats)
	}
	if a.peak.Load() > relevanceConcurrency {
		t.Fatalf("peak concurrency %d exceeds %d", a.peak.Load(), relevanceConcurrency)
	}
	// Kept results stay eligible; a fresh history must be answered from cache.
	fresh := oldToolHistory(20)
	_, stats := maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", maskGate{}, fresh)
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
	maskOldToolResultsCached(t.Context(), a.ask, &first.answers, "obj one", maskGate{}, oldToolHistory(4))
	if e.relevanceCacheFor("s", "obj one") != first {
		t.Fatal("same objective must reuse the cache")
	}
	second := e.relevanceCacheFor("s", "obj two")
	if second == first {
		t.Fatal("new objective must replace the cache")
	}
	if _, stats := maskOldToolResultsCached(t.Context(), a.ask, &second.answers, "obj two", maskGate{}, oldToolHistory(4)); stats.Asked != 4 || stats.Cached != 0 {
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
	maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", maskGate{}, oldToolHistory(3))
	maskOldToolResultsCached(t.Context(), a.ask, &cache, "obj", maskGate{}, oldToolHistory(3))
	if a.total() != 6 {
		t.Fatalf("failed answers must be re-asked, asks = %d", a.total())
	}
}

func TestCachedMaskingKeepsUnansweredResults(t *testing.T) {
	e, _ := testEngine(t)
	var warm sync.Map
	dropC1 := func(_ context.Context, _, _, args, _ string) (bool, bool) { return !strings.Contains(args, "c1"), true }
	maskOldToolResultsCached(t.Context(), dropC1, &warm, "obj", maskGate{}, oldToolHistory(2))
	r := e.relevanceCacheFor("s", "obj")
	warm.Range(func(k, v any) bool {
		if !v.(bool) {
			r.answers.Store(k, v)
		}
		return true
	})
	msgs := oldToolHistory(4)
	masked, stats, run := e.applyCachedMasking(t.Context(), "s", "obj", maskGate{}, msgs)
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
	e.backgroundWG.Wait()
	msgs, err := st.Messages(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	masked, stats, run := e.applyCachedMasking(ctx, s.ID, "obj", maskGate{}, msgs)
	if !masked || stats.Cached != 4 || stats.Cleared != 4 || run == nil || run.stats.Asked != 4 {
		t.Fatalf("masked=%v stats=%+v run=%+v", masked, stats, run)
	}
	if _, _, again := e.applyCachedMasking(ctx, s.ID, "obj", maskGate{}, msgs); again != nil {
		t.Fatal("a background pass must be reported once")
	}
}

func TestRelevanceFailsOpenWithoutClient(t *testing.T) {
	if relevanceKeeps(t.Context(), nil, "objective", "read", "{}", "content") {
		t.Fatal("disabled Jev relevance must not retain")
	}
}

// dropAllAsker always answers that a tool result can be cleared.
func dropAllAsker(_ context.Context, _, _, _, _ string) (bool, bool) { return false, true }

func TestMaskingDueThresholdAndGrowth(t *testing.T) {
	e, _ := testEngine(t)
	r := e.relevanceCacheFor("s", "obj")
	const window = 1000
	// Below the threshold, masking must not run.
	if e.maskingDue(r, window*maskThresholdNumerator/maskThresholdDenominator-1, window) {
		t.Fatal("masking must wait until the prompt passes the threshold fraction")
	}
	if !e.maskingDue(r, window*maskThresholdNumerator/maskThresholdDenominator, window) {
		t.Fatal("masking must be due once the threshold is crossed")
	}
	e.recordMasking(r, window*maskThresholdNumerator/maskThresholdDenominator)
	// Between crossings the history stays byte-identical: growth must reach the min-clear first.
	if e.maskingDue(r, window*maskThresholdNumerator/maskThresholdDenominator+maskMinClearTokens(window)-1, window) {
		t.Fatal("masking must stay idle until the prompt grows by the minimum-clear amount")
	}
	if !e.maskingDue(r, window*maskThresholdNumerator/maskThresholdDenominator+maskMinClearTokens(window), window) {
		t.Fatal("masking must be due again after the minimum-clear growth")
	}
	// After compaction the prompt shrinks; growth is measured afresh.
	e.recordMasking(r, window)
	if !e.maskingDue(r, window*maskThresholdNumerator/maskThresholdDenominator, window) {
		t.Fatal("masking must be due again once the prompt shrank below the last batch")
	}
}

func TestMaskingThresholdBelowCompaction(t *testing.T) {
	const window = 1000
	if window*maskThresholdNumerator/maskThresholdDenominator >= window*3/5 {
		t.Fatal("masking must become due before the compaction trigger, or compaction always runs on the same step")
	}
}

func TestMaskingMinimumClearSkipsSmallBatch(t *testing.T) {
	var cache sync.Map
	msgs := oldToolHistory(4)
	// Warm answers that would clear every eligible result.
	maskOldToolResultsCached(t.Context(), dropAllAsker, &cache, "obj", maskGate{}, oldToolHistory(4))
	before := store.JSON(msgs)
	// A gate that requires more tokens than this tiny history can free must leave it untouched.
	gate := maskGate{minTokens: 1_000_000}
	changed, stats := maskOldToolResultsCached(t.Context(), nil, &cache, "obj", gate, msgs)
	if changed || stats.Cleared != 0 || store.JSON(msgs) != before {
		t.Fatalf("minimum-clear must skip the rewrite: changed=%v stats=%+v", changed, stats)
	}
	// With a zero gate the same cached answers rewrite history.
	changed, stats = maskOldToolResultsCached(t.Context(), nil, &cache, "obj", maskGate{}, msgs)
	if !changed || stats.Cleared != 4 {
		t.Fatalf("ungated pass must clear: changed=%v stats=%+v", changed, stats)
	}
}

func TestMaskingKeepsEditAndVerificationExemptions(t *testing.T) {
	var msgs []store.Message
	// Old edit + verification exec, then many plain reads so the exemptions sit past the recent horizon.
	for _, tc := range []struct{ id, name, arg string }{
		{"edit1", "edit", "path"},
		{"ver1", "exec", "go test ./..."},
	} {
		msgs = append(msgs,
			store.Message{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: tc.id, Name: tc.name, Arguments: map[string]any{tc.arg: "x"}}}})},
			store.Message{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: tc.id, Content: "protected-" + tc.id})},
		)
	}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("r%d", i)
		msgs = append(msgs,
			store.Message{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: id, Name: "read", Arguments: map[string]any{"path": id}}}})},
			store.Message{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: id, Content: "drop-" + id})},
		)
	}
	for i := 0; i < 11; i++ {
		msgs = append(msgs, store.Message{Role: "assistant", ContentJSON: "{}"})
	}
	changed, stats := maskOldToolResultsCached(t.Context(), dropAllAsker, nil, "obj", maskGate{}, msgs)
	if !changed || stats.Cleared != 6 {
		t.Fatalf("expected only the plain reads cleared: changed=%v stats=%+v", changed, stats)
	}
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.ContentJSON, "protected-edit1") || strings.Contains(m.ContentJSON, "protected-ver1") {
			if strings.Contains(m.ContentJSON, "cleared") {
				t.Fatalf("exemption cleared: %s", m.ContentJSON)
			}
			continue
		}
		if !strings.Contains(m.ContentJSON, "cleared") {
			t.Fatalf("eligible result not cleared: %s", m.ContentJSON)
		}
	}
}

func TestMaskingHistoryByteIdenticalBetweenCrossings(t *testing.T) {
	e, _ := testEngine(t)
	r := e.relevanceCacheFor("s", "obj")
	history := func() []store.Message {
		pad := strings.Repeat("x", 400)
		var out []store.Message
		for i := 0; i < 8; i++ {
			id := fmt.Sprintf("c%d", i)
			out = append(out,
				store.Message{Role: "assistant", ContentJSON: store.JSON(provider.Message{ToolCalls: []provider.ToolCall{{ID: id, Name: "read", Arguments: map[string]any{"path": id}}}})},
				store.Message{Role: "tool", ContentJSON: store.JSON(provider.Message{ToolCallID: id, Content: pad + id})},
			)
		}
		for i := 0; i < 11; i++ {
			out = append(out, store.Message{Role: "assistant", ContentJSON: "{}"})
		}
		return out
	}
	msgs := history()
	// Warm the session cache directly; applyCachedMasking reads the same answers.
	maskOldToolResultsCached(t.Context(), dropAllAsker, &r.answers, "obj", maskGate{}, history())
	window := 1000
	// First crossing rewrites.
	if !e.maskingDue(r, window, window) {
		t.Fatal("expected due at full window")
	}
	changed, _, _ := e.applyCachedMasking(t.Context(), "s", "obj", maskGateFor(window), msgs)
	if !changed {
		t.Fatal("first crossing must rewrite")
	}
	e.recordMasking(r, window)
	// Between crossings the same pass must leave history untouched.
	before := store.JSON(msgs)
	if e.maskingDue(r, window+1, window) {
		t.Fatal("must not be due between crossings")
	}
	// Even a direct gated apply with the same answers must refuse a tiny clear when minTokens is high.
	changed, _ = maskOldToolResultsCached(t.Context(), nil, &r.answers, "obj", maskGate{minTokens: 1_000_000}, msgs)
	if changed || store.JSON(msgs) != before {
		t.Fatal("history must stay byte-identical between crossings")
	}
}

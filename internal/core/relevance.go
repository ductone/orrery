package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

var relevanceQuestion = map[string]jev.Question{"needed": jev.Noul(
	"Will the agent need this result's content, not just that it ran, to finish the current objective?",
	"The content itself is needed.",
	"A stub that it ran is enough.",
)}

func relevanceKeeps(ctx context.Context, client *jev.Client, objective, tool, args, content string) bool {
	keep, _ := relevanceAnswer(ctx, client, objective, tool, args, content)
	return keep
}

// relevanceAnswer asks Jev whether an old tool result is still needed. ok is
// false when no answer was obtained, so callers must not cache the result.
func relevanceAnswer(ctx context.Context, client *jev.Client, objective, tool, args, content string) (keep, ok bool) {
	if client == nil {
		return false, false
	}
	state := map[string]any{"objective": truncate(objective, 2_000), "tool": tool, "arguments": truncate(args, 1_000), "content": truncate(content, 4_000)}
	resp, err := client.Ask(ctx, state, relevanceQuestion)
	if err != nil || resp.Answers["needed"].Noul == nil {
		return false, false
	}
	return *resp.Answers["needed"].Noul >= 0.7, true
}

// relevanceAsker decides keep/clear for one old tool result; ok reports
// whether the answer is real and may be cached.
type relevanceAsker func(ctx context.Context, objective, tool, args, content string) (keep, ok bool)

func jevRelevanceAsker(client *jev.Client) relevanceAsker {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, objective, tool, args, content string) (bool, bool) {
		return relevanceAnswer(ctx, client, objective, tool, args, content)
	}
}

// relevanceConcurrency bounds parallel relevance questions in one masking pass.
const relevanceConcurrency = 8

// maskThresholdNumerator/Denominator: a masking batch is only considered once
// the prompt passes this fraction of the effective window. maskMinClearDivisor
// sets how many tokens a batch must clear to be worth the prefix-cache break.
const (
	maskThresholdNumerator   = 3
	maskThresholdDenominator = 5
	maskMinClearDivisor      = 20
	minMaskCandidates        = 4
)

// maskGate bounds a batching masking pass: a pass must clear at least minTokens
// estimated tokens and cover at least minMaskCandidates results to be worth a
// prefix-cache break. Its zero value disables gating, so callers that ask Jev
// directly keep the pre-batching behavior.
type maskGate struct{ minTokens int }

// maskGateFor gates a batch at a fixed fraction of the effective window.
func maskGateFor(window int) maskGate { return maskGate{minTokens: maskMinClearTokens(window)} }

// allows reports whether clearing tokens over candidates results justifies
// rewriting stored history.
func (g maskGate) allows(candidates, tokens int) bool {
	return g.minTokens <= 0 || (candidates >= minMaskCandidates && tokens >= g.minTokens)
}

// maskMinClearTokens is the smallest clearing that justifies a history rewrite.
func maskMinClearTokens(window int) int { return max(1, window/maskMinClearDivisor) }

type maskStats struct {
	Candidates, Asked, Cached, Cleared int
	// Removed counts the characters this pass cleared and Earliest is the oldest
	// message position it touched (-1 when it cleared nothing).
	Removed  int
	Earliest int
}

// TokensRemoved estimates the prompt tokens this pass removed.
func (s maskStats) TokensRemoved() int { return s.Removed / 4 }

// sessionRelevance holds keep/clear answers for one session's current
// objective. A new objective replaces it, invalidating every answer.
type sessionRelevance struct {
	objective string
	answers   sync.Map // tool call id + call -> bool
	running   atomic.Bool
	// gated records that a batch was already applied: further batches wait
	// for the prompt to grow by the minimum-clear amount before re-crossing.
	gated  bool
	gateAt int

	mu      sync.Mutex
	pending *maskRun // finished background pass not yet reported
}

// maskRun records one background masking pass for the context.masking event.
type maskRun struct {
	stats    maskStats
	duration time.Duration
}

func (e *Engine) relevanceCacheFor(sid, objective string) *sessionRelevance {
	if v, ok := e.relevanceCaches.Load(sid); ok {
		if r := v.(*sessionRelevance); r.objective == objective {
			return r
		}
	}
	r := &sessionRelevance{objective: objective}
	e.relevanceCaches.Store(sid, r)
	return r
}

// maskingAsker asks Jev; with Jev disabled every old result is cleared, as
// before caching, and that answer is cacheable.
func (e *Engine) maskingAsker() relevanceAsker {
	if ask := jevRelevanceAsker(e.relevanceClient()); ask != nil {
		return ask
	}
	return func(context.Context, string, string, string, string) (bool, bool) { return false, true }
}

// scheduleMasking warms the relevance cache in the background so the next
// turn boundary can apply masking without waiting on Jev. At most one pass
// runs per session; a pass that is already running is not restarted.
func (e *Engine) scheduleMasking(ctx context.Context, sid, objective string) {
	r := e.relevanceCacheFor(sid, objective)
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	ask := e.maskingAsker()
	e.shadowWG.Add(1)
	go func() {
		defer e.shadowWG.Done()
		defer r.running.Store(false)
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		msgs, err := e.store.Messages(bg, sid)
		if err != nil {
			return
		}
		start := time.Now()
		_, stats := maskOldToolResultsCached(bg, ask, &r.answers, objective, maskGate{}, msgs)
		if stats.Candidates == 0 {
			return
		}
		r.mu.Lock()
		r.pending = &maskRun{stats: stats, duration: time.Since(start)}
		r.mu.Unlock()
	}()
}

// applyCachedMasking clears old results using only cached answers, so it never
// blocks on Jev. It also returns the last finished background pass, if any.
func (e *Engine) applyCachedMasking(ctx context.Context, sid, objective string, gate maskGate, msgs []store.Message) (bool, maskStats, *maskRun) {
	r := e.relevanceCacheFor(sid, objective)
	r.mu.Lock()
	run := r.pending
	r.pending = nil
	r.mu.Unlock()
	masked, stats := maskOldToolResultsCached(ctx, nil, &r.answers, objective, gate, msgs)
	return masked, stats, run
}

// maskingDue reports whether the prompt has grown past the point where a
// history rewrite is worth a prefix-cache break. A session that already
// applied a batch must first grow by the minimum-clear amount again, so the
// stored history stays byte-identical between crossings.
func (e *Engine) maskingDue(r *sessionRelevance, inputTokens, window int) bool {
	if window <= 0 {
		return false
	}
	if inputTokens < window*maskThresholdNumerator/maskThresholdDenominator {
		return false
	}
	return !r.gated || inputTokens-r.gateAt >= maskMinClearTokens(window)
}

// recordMasking notes where a batch was applied, so the next one waits for the
// prompt to grow by the minimum-clear amount.
func (e *Engine) recordMasking(r *sessionRelevance, inputTokens int) {
	r.gated, r.gateAt = true, inputTokens
}

// maskOldToolResultsCached clears old tool results the agent no longer needs.
// Answers are reused from cache (keyed by tool call within the cache's
// objective); uncached results are asked in parallel with bounded concurrency.
// With a cache but no asker, uncached results are kept until answered.
func maskOldToolResultsCached(ctx context.Context, ask relevanceAsker, cache *sync.Map, objective string, gate maskGate, msgs []store.Message) (bool, maskStats) {
	type candidate struct {
		index           int
		name, args, key string
		parsed          provider.Message
		keep            bool
		answered        bool
	}
	var cands []candidate
	assistants := 0
	keepVerification := true
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			assistants++
		}
		if msgs[i].Role != "tool" || assistants <= 10 {
			continue
		}
		var parsed provider.Message
		if json.Unmarshal([]byte(msgs[i].ContentJSON), &parsed) != nil || strings.Contains(parsed.Content, "\"cleared\":true") {
			continue
		}
		name, args := matchingCall(msgs, i, parsed.ToolCallID)
		if name == "edit" || (keepVerification && name == "exec" && (strings.Contains(args, "test") || strings.Contains(args, "build"))) {
			keepVerification = name != "exec"
			continue
		}
		key := ""
		if cache != nil && parsed.ToolCallID != "" {
			key = objective + "\x00" + parsed.ToolCallID + "\x00" + name + "\x00" + args
		}
		cands = append(cands, candidate{index: i, name: name, args: args, key: key, parsed: parsed})
	}
	stats := maskStats{Candidates: len(cands), Earliest: -1}
	// A gated pass plans its clearing before touching history, so a batch too
	// small to pay for the prefix-cache break leaves the messages byte-identical.
	if gate.minTokens > 0 {
		planned, tokens := 0, 0
		for i := range cands {
			c := &cands[i]
			if cache == nil || c.key == "" {
				continue
			}
			v, ok := cache.Load(c.key)
			if !ok {
				continue
			}
			c.keep = v.(bool)
			c.answered = true
			stats.Cached++
			if !c.keep {
				planned++
				replacement := fmt.Sprintf("{\"cleared\":true,\"tool\":%q,\"original_chars\":%d,\"hint\":\"re-run to see it\"}", c.name, len(c.parsed.Content))
				tokens += (len(c.parsed.Content) - len(replacement)) / 4
			}
		}
		if !gate.allows(planned, tokens) {
			return false, stats
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, relevanceConcurrency)
	for i := range cands {
		c := &cands[i]
		if c.answered {
			continue
		}
		if c.key != "" {
			if v, ok := cache.Load(c.key); ok {
				c.keep = v.(bool)
				stats.Cached++
				continue
			}
		}
		if ask == nil {
			c.keep = cache != nil
			continue
		}
		stats.Asked++
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			keep, ok := ask(ctx, objective, c.name, c.args, c.parsed.Content)
			c.keep = keep
			if ok && c.key != "" {
				cache.Store(c.key, keep)
			}
		}()
	}
	wg.Wait()
	changed := false
	// Candidates were collected newest first; clear oldest eligible results first.
	for i := len(cands) - 1; i >= 0; i-- {
		c := cands[i]
		if c.keep {
			continue
		}
		before := len(c.parsed.Content)
		c.parsed.Content = fmt.Sprintf("{\"cleared\":true,\"tool\":%q,\"original_chars\":%d,\"hint\":\"re-run to see it\"}", c.name, before)
		msgs[c.index].ContentJSON = store.JSON(c.parsed)
		changed = true
		stats.Cleared++
		stats.Removed += before - len(c.parsed.Content)
		if stats.Earliest < 0 || c.index < stats.Earliest {
			stats.Earliest = c.index
		}
	}
	return changed, stats
}

func lostFact(ctx context.Context, client *jev.Client, state DurableState, fact string) bool {
	if client == nil || strings.TrimSpace(fact) == "" {
		return false
	}
	resp, err := client.Ask(ctx, map[string]any{"summary": truncate(store.JSON(state), 24_000), "fact": fact}, map[string]jev.Question{"preserved": jev.Noul("Does the summary preserve this fact?", "It preserves it.", "It drops it.")})
	return err == nil && resp.Answers["preserved"].Noul != nil && *resp.Answers["preserved"].Noul < 0.3
}

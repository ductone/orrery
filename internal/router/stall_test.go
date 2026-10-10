package router

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
)

type warmStallLedger struct{ ledger }

func (l *warmStallLedger) Cache(context.Context, string, string) (store.CacheEntry, error) {
	return store.CacheEntry{WarmPrefixTokens: 1000, LastHit: time.Now(), TTL: time.Hour}, nil
}

func TestStallReasonsIgnoreTurnCounters(t *testing.T) {
	cases := []struct {
		signals StallSignals
		reason  string
	}{
		{StallSignals{PhaseTurns: 100, NoProgressTurns: 100}, ""},
		{StallSignals{FailedCommands: 2}, "failed commands"},
		{StallSignals{TestFailStreak: 2}, "failing tests"},
		{StallSignals{RepeatedEdits: 3}, "repeated edits"},
		{StallSignals{RepeatedReads: 2}, "repeated reads"},
		{StallSignals{RepeatedSearches: 2}, "repeated searches"},
	}
	for _, c := range cases {
		if got := stallReason(c.signals); got != c.reason {
			t.Fatalf("%+v: reason=%q, want %q", c.signals, got, c.reason)
		}
	}
}

func TestLongCleanPhaseAndStallRecovery(t *testing.T) {
	for _, phase := range []Phase{Implement, Explore, WrapUp} {
		t.Run(string(phase), func(t *testing.T) {
			l := &warmStallLedger{}
			p := NewV1(config.RouterConfig{LambdaCost: .35}, l)
			state := RoutingState{SessionID: "s", Point: TurnStart, Phase: phase, InputTokens: 1000, EstimatedOutput: 100, AvailableModels: []string{"openai/gpt-5.6-sol", "openai/gpt-5.6-terra"}, Stall: StallSignals{PhaseTurns: 100, NoProgressTurns: 100}}
			d, _, err := p.Decide(context.Background(), state)
			if err != nil {
				t.Fatal(err)
			}
			if d.Model.Tier != model.Efficient || d.StallBoost != "" {
				t.Fatalf("clean long phase: %+v", d)
			}
			state.Stall.FailedCommands = 2
			d, _, err = p.Decide(context.Background(), state)
			if err != nil {
				t.Fatal(err)
			}
			if d.Model.Tier != model.Frontier || d.StallBoost != "failed commands" {
				t.Fatalf("two failures: %+v", d)
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(store.JSON(d)), &event); err != nil {
				t.Fatal(err)
			}
			if event["stall_boost"] != "failed commands" {
				t.Fatalf("decision reason missing: %+v", event)
			}
			state.CurrentModel = d.Model.ID
			state.ToolContinuation = true
			state.Stall.FailedCommands = 0
			// Without recovery, only the small tool-continuation margin protects the
			// frontier incumbent; the work-cost estimate decides, and the efficient
			// model clears the margin.
			d, _, err = p.Decide(context.Background(), state)
			if err != nil {
				t.Fatal(err)
			}
			if d.Model.Tier != model.Efficient {
				t.Fatalf("a warm frontier incumbent must not be kept by stickiness alone: %+v", d)
			}
			for _, c := range d.Candidates {
				if c.Rejected == "" && c.Model != state.CurrentModel && c.SwitchPenalty != toolContinuationSwitchPenalty {
					t.Fatalf("mid-chain switch penalty %.2f, want %.2f", c.SwitchPenalty, toolContinuationSwitchPenalty)
				}
			}
			state.Stall.Deescalated = true
			d, _, err = p.Decide(context.Background(), state)
			if err != nil {
				t.Fatal(err)
			}
			if d.Model.Tier != model.Efficient || d.StallBoost != "" {
				t.Fatalf("cleared failures must return to efficient: %+v", d)
			}
			for _, c := range d.Candidates {
				if c.Rejected == "" && c.SwitchPenalty != 0 {
					t.Fatalf("recovery retained cache penalty: %+v", c)
				}
			}
		})
	}
}

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
)

func convergenceEngine(t *testing.T, enough, fresh float64, status int) (*Engine, *atomic.Int32) {
	t.Helper()
	e, _ := testEngine(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"enough_evidence": map[string]any{"type": "noul", "noul": enough},
			"new_ground":      map[string]any{"type": "noul", "noul": fresh},
		}})
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: srv.URL, Review: true}}
	e.ReplaceRuntime(cfg, nil, nil)
	return e, &calls
}

var readWorker = agentproto.TaskRequest{Workspace: agentproto.Workspace{Mode: "read"}}

func TestWorkerTurnLimits(t *testing.T) {
	if soft, hard := workerTurnLimits(readWorker); soft != 4 || hard != 6 {
		t.Fatalf("default limits = %d/%d", soft, hard)
	}
	req := readWorker
	req.Hints.WorkerTurns = 10
	if soft, hard := workerTurnLimits(req); soft != 10 || hard != 15 {
		t.Fatalf("planned limits = %d/%d", soft, hard)
	}
}

func TestSynthesisDue(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		enough, fresh float64
		status        int
		turn          int
		want          bool
		wantCalls     int32
	}{
		{"before the soft limit", 0.1, 0.9, 200, 3, false, 0},
		{"still gathering new ground", 0.4, 0.8, 200, 4, false, 1},
		{"enough evidence", 0.85, 0.8, 200, 4, true, 1},
		{"circling", 0.4, 0.1, 200, 5, true, 1},
		{"classifier down", 0.4, 0.8, 529, 4, true, 1},
		{"hard limit overrides the classifier", 0.1, 0.9, 200, 6, true, 0},
	} {
		e, calls := convergenceEngine(t, tc.enough, tc.fresh, tc.status)
		if got := e.synthesisDue(ctx, "s", "task", readWorker, tc.turn, nil, nil); got != tc.want || calls.Load() != tc.wantCalls {
			t.Errorf("%s: due=%v calls=%d, want %v/%d", tc.name, got, calls.Load(), tc.want, tc.wantCalls)
		}
	}
}

func TestSynthesisDueWithoutJevUsesTheSoftLimit(t *testing.T) {
	e, _ := testEngine(t)
	if e.synthesisDue(context.Background(), "s", "t", readWorker, 3, nil, nil) || !e.synthesisDue(context.Background(), "s", "t", readWorker, 4, nil, nil) {
		t.Fatal("without Jev, synthesis is forced exactly at the soft limit")
	}
	write := agentproto.TaskRequest{Workspace: agentproto.Workspace{Mode: "shared-write"}}
	if e.synthesisDue(context.Background(), "s", "t", write, 50, nil, nil) {
		t.Fatal("only read workers are asked to synthesise")
	}
}

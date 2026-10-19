package core

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

// wrappedStore is a storage.Store that is not the SQLite type, so an engine
// using it can only reach the store through the interface.
type wrappedStore struct {
	storage.Store
	messages atomic.Int64
}

func (w *wrappedStore) AddMessage(ctx context.Context, sid, role string, content any) error {
	w.messages.Add(1)
	return w.Store.AddMessage(ctx, sid, role, content)
}

// TestEngineRunsOnAnyStore runs a full task on an engine whose store is only
// known through storage.Store.
func TestEngineRunsOnAnyStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ws := &wrappedStore{Store: st}

	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 5, JobDefaultFraction: .2}
	e := New(cfg, ws, provider.New(cfg), nil)
	t.Cleanup(func() { e.Close() })

	req := agentproto.TaskRequest{Spec: "task", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	res, err := e.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Pass {
		t.Fatalf("result = %+v", res)
	}
	if ws.messages.Load() == 0 {
		t.Fatal("the engine wrote no messages through the wrapped store")
	}
	// The engine attributes the run's outcome to the route that answered.
	got, err := ws.ModelOutcomes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	passes := 0
	for _, o := range got {
		passes += o.RunPass
	}
	if passes != 1 {
		t.Fatalf("model outcomes = %+v, want one run pass", got)
	}
}

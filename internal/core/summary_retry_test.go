package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func summaryEngine(t *testing.T, replies ...map[string]any) (*Engine, *store.Store, *[]float64) {
	t.Helper()
	e, st := testEngine(t)
	var limits []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		limits = append(limits, body["max_output_tokens"].(float64))
		_ = json.NewEncoder(w).Encode(replies[min(len(limits), len(replies))-1])
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "k", BaseURL: srv.URL}}}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	return e, st, &limits
}

func summaryReply(text, status, reason string) map[string]any {
	reply := map[string]any{"model": "gpt-5.6-terra", "status": status, "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 500}}
	if reason != "" {
		reply["incomplete_details"] = map[string]any{"reason": reason}
	}
	return reply
}

const validSummary = `{"objective":"ship it","current_objective":"ship it","pending_report":"","resolved_requests":[],"requirements":["r"],"decisions":[],"completed":[],"files":[],"verification":[],"open_work":[],"blockers":[],"instructions":[],"worker_results":[]}`

func TestTruncatedSummaryIsRetriedWithMoreRoom(t *testing.T) {
	e, st, limits := summaryEngine(t, summaryReply(`{"objective":"ship`, "incomplete", "max_output_tokens"), summaryReply(validSummary, "completed", ""))
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Spec: "ship it", Model: "openai/gpt-5.6-terra", BudgetUSD: 5}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	state, meta, err := e.semanticSummary(ctx, s, nil, store.Continuation{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Objective != "ship it" || len(*limits) != 2 || (*limits)[0] != float64(summaryOutputLimits[0]) || (*limits)[1] != float64(summaryOutputLimits[1]) {
		t.Fatalf("state=%+v limits=%v", state, *limits)
	}
	after, _ := st.Session(ctx, s.ID)
	// Each reply reports the same usage, so the total is twice one attempt.
	oneAttempt := meta["cost_usd"].(float64) / 2
	if oneAttempt <= 0 || after.SpentUSD < 2*oneAttempt-1e-12 {
		t.Fatalf("both attempts must be charged: spent %v, reported %v", after.SpentUSD, meta["cost_usd"])
	}
}

func TestSummaryThatIsInvalidButNotTruncatedIsNotRetried(t *testing.T) {
	e, st, limits := summaryEngine(t, summaryReply(`not json`, "completed", ""))
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Spec: "x", Model: "openai/gpt-5.6-terra", BudgetUSD: 5}
	_ = st.CreateSession(ctx, s)
	_, _, err := e.semanticSummaryWithModel(ctx, s, nil, store.Continuation{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `stop reason "completed"`) || len(*limits) != 1 {
		t.Fatalf("err=%v limits=%v", err, *limits)
	}
	if after, _ := st.Session(ctx, s.ID); after.SpentUSD <= 0 {
		t.Fatal("a failed summary attempt still costs money and must be charged")
	}
}

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestCompactionSideModelFallback(t *testing.T) {
	for _, mode := range []string{"cheap", "unavailable", "invalid", "error"} {
		t.Run(mode, func(t *testing.T) {
			e, st := testEngine(t)
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				id := body["model"].(string)
				calls = append(calls, id)
				if id == "gpt-5.6-terra" && mode == "error" {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				text := validSummary
				if id == "gpt-5.6-terra" && mode == "invalid" {
					text = "not json"
				}
				_ = json.NewEncoder(w).Encode(summaryReply(text, "completed", ""))
			}))
			t.Cleanup(srv.Close)
			cfg := config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "k", BaseURL: srv.URL}}}
			registry := provider.New(cfg)
			if mode == "unavailable" {
				registry.MarkUnavailable("openai/gpt-5.6-terra")
			}
			e.ReplaceRuntime(cfg, registry, nil)
			ctx := context.Background()
			s := store.Session{ID: uuid.NewString(), Spec: "ship it", Model: "openai/gpt-5.6-sol", BudgetUSD: 5}
			if err := st.CreateSession(ctx, s); err != nil {
				t.Fatal(err)
			}
			state, meta, err := e.semanticSummary(ctx, s, nil, store.Continuation{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := []string{"gpt-5.6-terra"}
			wantModel := "openai/gpt-5.6-terra"
			if mode != "cheap" {
				wantModel = s.Model
				wantCalls = []string{"gpt-5.6-sol"}
				if mode != "unavailable" {
					wantCalls = append([]string{"gpt-5.6-terra"}, wantCalls...)
				}
			}
			if state.Objective != "ship it" || meta["model"] != wantModel || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("state=%+v meta=%v calls=%v, want model=%s calls=%v", state, meta, calls, wantModel, wantCalls)
			}
			after, err := st.Session(ctx, s.ID)
			if err != nil || after.Model != s.Model || after.SpentUSD <= 0 {
				t.Fatalf("session=%+v err=%v", after, err)
			}
			if mode == "invalid" && after.SpentUSD <= meta["cost_usd"].(float64) {
				t.Fatal("failed cheap summary must also be charged")
			}
		})
	}
}

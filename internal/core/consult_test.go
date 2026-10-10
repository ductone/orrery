package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/classify"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestOpeningRequestIsClassified(t *testing.T) {
	e, _ := testEngine(t)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{Routing: true}}, nil, nil)
	var seen string
	e.UseClassifier(&classify.Fake{Answer: func(state any, _ string, _ classify.Question) (classify.Answer, error) {
		seen, _ = state.(map[string]any)["message"].(string)
		return classify.Answer{Type: "choice", Choice: "implement", Probabilities: map[string]float64{"implement": .7}}, nil
	}})
	s := store.Session{ID: uuid.NewString(), Spec: "Fix the off-by-one in Retry", Phase: "plan"}
	got := e.instructionPhase(context.Background(), s, nil, nil)
	if got.Phase != router.Implement || seen != s.Spec {
		t.Fatalf("choice = %+v, classifier saw %q", got, seen)
	}
}

func TestConsultAsksAStrongerModelWithTheSession(t *testing.T) {
	e, st := testEngine(t)
	workspace, git := gitRepo(t)
	writeFile(t, workspace, "README.md", "base\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	turns := 0
	var mu sync.Mutex
	var consultBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		instructions, _ := body["instructions"].(string)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(instructions, "senior engineer consulted"):
			consultBody = body
			_ = json.NewEncoder(w).Encode(responsesText("Use a cursor that encodes the last ID; reject unknown cursors with 400."))
		case !strings.Contains(instructions, systemPromptLead):
			_ = json.NewEncoder(w).Encode(responsesText("Title"))
		default:
			turns++
			if turns == 1 {
				_ = json.NewEncoder(w).Encode(responsesCall("c1", "consult", map[string]any{"question": "Offset or cursor pagination for a list that changes while paging?"}))
				return
			}
			_ = json.NewEncoder(w).Encode(responsesText("Recommended cursor pagination, as advised."))
		}
	}))
	t.Cleanup(srv.Close)
	cfg := gateConfig(workspace, srv.URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{Spec: "How should we paginate the items API?", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	sid, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.Status != agentproto.Pass {
			t.Fatalf("result = %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	if consultBody == nil {
		t.Fatal("the consulted model was never called")
	}
	raw, _ := json.Marshal(consultBody)
	if !strings.Contains(string(raw), "Offset or cursor pagination") || !strings.Contains(string(raw), "How should we paginate") || consultBody["tools"] != nil {
		t.Fatalf("the consultation must carry the question and the session: %s", raw)
	}
	var answered, advised bool
	es, _ := st.EventsAfter(context.Background(), sid, 0)
	for _, ev := range es {
		answered = answered || ev.Type == "consult.answered"
		advised = advised || ev.Type == "tool.finished" && strings.Contains(string(ev.Data), "encodes the last ID")
	}
	if !answered || !advised {
		t.Fatalf("answered=%v advised=%v: the advice must reach the agent as the tool result", answered, advised)
	}
}

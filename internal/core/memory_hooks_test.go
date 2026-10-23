package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

type retrievedPayload struct {
	Version          int      `json:"version"`
	IDs              []string `json:"ids"`
	Ranks            []int    `json:"ranks"`
	Statuses         []string `json:"statuses"`
	Scopes           []string `json:"scopes"`
	Source           string   `json:"source"`
	SelectorFallback bool     `json:"selector_fallback"`
	Injected         bool     `json:"injected"`
	Reason           string   `json:"reason"`
	Count            int      `json:"count"`
	Outcome          string   `json:"outcome"`
	CacheBoundaryID  string   `json:"cache_boundary_id"`
}

// lastRetrieved returns the most recent memory.retrieved event recorded in sid.
func lastRetrieved(t *testing.T, e *Engine, sid string) retrievedPayload {
	t.Helper()
	events, err := e.store.EventsAfter(context.Background(), sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "memory.retrieved" {
			var p retrievedPayload
			if err := json.Unmarshal(events[i].Data, &p); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("no memory.retrieved event recorded")
	return retrievedPayload{}
}

func commitMemory(t *testing.T, e *Engine, w store.Workspace, text, status string, confidence float64) store.MemoryRecord {
	t.Helper()
	rec, err := e.store.CommitMemory(context.Background(), store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: text, Provenance: "extracted", Confidence: confidence, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestMemoryDefaultSelectionUnchangedWithoutSelector(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.Default().Memory)
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	commitMemory(t, e, w, "Run go test ./... before pushing", "active", 0.9)
	commitMemory(t, e, w, "Unrelated note about lint", "active", 0.5)
	commitMemory(t, e, w, "Pending note about tests", "pending", 0.6)
	active, _ := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, Status: "active"})
	want := rankMemory(active, "tests", e.cfg.Memory.Records(), e.cfg.Memory.Tokens(), e.cfg.Memory.RecordBytes())
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundarySessionStart, 1, nil)
	ep, _ := e.pinnedMemory("s1")
	if len(ep.records) != len(want) {
		t.Fatalf("pinned %d records, want %d", len(ep.records), len(want))
	}
	for i := range want {
		if ep.records[i].ID != want[i].ID {
			t.Fatalf("order differs at %d", i)
		}
	}
	if strings.Contains(ep.rendered, "Pending note") || strings.Contains(ep.rendered, "[unconfirmed]") {
		t.Fatalf("default injected pending memory: %s", ep.rendered)
	}
	p := lastRetrieved(t, e, "s1")
	if p.Version != 2 || p.Source != "default" || p.SelectorFallback || !p.Injected || p.Reason != "ranked" {
		t.Fatalf("payload: %+v", p)
	}
	if !slices.Equal(p.Ranks, []int{1, 2}) || !slices.Equal(p.Statuses, []string{"active", "active"}) || p.Count != 2 {
		t.Fatalf("payload: %+v", p)
	}
}

func TestMemorySelectorOrderLimitsAndPendingMarker(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default().Memory
	cfg.MaxRecords = 3
	e := memoryEngine(t, cfg)
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	a := commitMemory(t, e, w, "Alpha convention", "active", 0.9)
	b := commitMemory(t, e, w, "Bravo convention", "active", 0.9)
	big := commitMemory(t, e, w, strings.Repeat("huge ", 400), "active", 0.9)
	p := commitMemory(t, e, w, "Pending convention", "pending", 0.6)
	c := commitMemory(t, e, w, "Charlie convention", "active", 0.9)
	gone := commitMemory(t, e, w, "Forgotten convention", "active", 0.9)
	if err := e.store.ForgetMemory(ctx, w.ID, gone.ID); err != nil {
		t.Fatal(err)
	}
	e.cfg.Memory.MaxTokens = estimate(a.Text) + estimate(b.Text) + estimate(p.Text) + estimate(c.Text)
	var reqs []MemorySelectRequest
	e.memorySelector = func(_ context.Context, req MemorySelectRequest) ([]storage.MemoryRecord, error) {
		reqs = append(reqs, req)
		byID := map[string]storage.MemoryRecord{}
		for _, c := range req.Candidates {
			byID[c.ID] = c
		}
		// unknown and forgotten IDs are ignored, the repeat dropped, the
		// over-budget record skipped, and the limit stops after three.
		return []storage.MemoryRecord{{ID: "nope", Text: "injected by host"}, byID[p.ID], {ID: gone.ID}, byID[big.ID], byID[b.ID], byID[p.ID], byID[a.ID], {ID: "x"}, byID[c.ID]}, nil
	}
	e.refreshMemory(ctx, "s1", root, "the spec", "implement", memoryBoundarySessionStart, 1, nil)
	if len(reqs) != 1 {
		t.Fatalf("selector calls = %d", len(reqs))
	}
	req := reqs[0]
	if req.SessionID != "s1" || req.WorkspaceID != w.ID || req.Spec != "the spec" || req.Phase != "implement" || req.Limit != 3 || req.TokenBudget != e.cfg.Memory.Tokens() {
		t.Fatalf("request: %+v", req)
	}
	var cand []string
	for _, c := range req.Candidates {
		cand = append(cand, c.ID)
		if c.Status != "active" && c.Status != "pending" {
			t.Fatalf("candidate status %q", c.Status)
		}
	}
	if len(cand) != 5 || slices.Contains(cand, gone.ID) || !slices.Contains(cand, p.ID) {
		t.Fatalf("candidates: %v", cand)
	}
	ep, _ := e.pinnedMemory("s1")
	var got []string
	for _, r := range ep.records {
		got = append(got, r.ID)
	}
	if !slices.Equal(got, []string{p.ID, b.ID, a.ID}) {
		t.Fatalf("pinned = %v, want pending, bravo, alpha", got)
	}
	if strings.Contains(ep.rendered, "injected by host") || !strings.Contains(ep.rendered, "- [fact/agent] [unconfirmed] Pending convention") || !strings.Contains(ep.rendered, "- [fact/agent] Bravo convention") {
		t.Fatalf("rendered: %s", ep.rendered)
	}
	pl := lastRetrieved(t, e, "s1")
	if pl.Source != "selector" || pl.Reason != "selected" || !slices.Equal(pl.IDs, got) || !slices.Equal(pl.Ranks, []int{1, 3, 4}) || !slices.Equal(pl.Statuses, []string{"pending", "active", "active"}) {
		t.Fatalf("payload: %+v", pl)
	}

	// The pinned set is kept for the session; only compaction re-selects.
	e.refreshMemory(ctx, "s1", root, "the spec", "implement", memoryBoundaryPhaseTransition, 2, nil)
	e.invalidateMemoryWorkspace(w.ID)
	e.refreshMemory(ctx, "s1", root, "the spec", "implement", memoryBoundaryPhaseTransition, 3, nil)
	e.refreshMemory(ctx, "s1", root, "the spec", "review", memoryBoundaryPhaseTransition, 4, nil)
	if len(reqs) != 1 {
		t.Fatalf("selector calls after stale and phase change = %d, want 1", len(reqs))
	}
	e.setCacheBoundary("s1", "s1:compaction")
	e.refreshMemory(ctx, "s1", root, "the spec", "review", memoryBoundaryCompaction, 5, nil)
	if len(reqs) != 2 {
		t.Fatalf("selector calls after compaction = %d, want 2", len(reqs))
	}
}

func TestMemorySelectorErrorFallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.Default().Memory)
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	a := commitMemory(t, e, w, "Active convention", "active", 0.9)
	commitMemory(t, e, w, "Pending convention", "pending", 0.6)
	e.memorySelector = func(context.Context, MemorySelectRequest) ([]storage.MemoryRecord, error) {
		return nil, errors.New("host down")
	}
	e.refreshMemory(ctx, "s1", root, "convention", "implement", memoryBoundarySessionStart, 1, nil)
	ep, _ := e.pinnedMemory("s1")
	if len(ep.records) != 1 || ep.records[0].ID != a.ID {
		t.Fatalf("fallback pinned: %+v", ep.records)
	}
	pl := lastRetrieved(t, e, "s1")
	if pl.Source != "default" || !pl.SelectorFallback || !slices.Equal(pl.IDs, []string{a.ID}) {
		t.Fatalf("payload: %+v", pl)
	}
}

func TestMemoryProfileCriteriaReachTriageAndExtraction(t *testing.T) {
	e := memoryEngine(t, config.MemoryConfig{})
	if e.memoryExtractionSystem() != memoryExtractionPrompt {
		t.Fatal("default extraction prompt changed")
	}
	e.profile = Profile{MemoryDurable: "Customer escalation contacts", MemoryNotDurable: "One-off ticket details"}
	if r := e.profile.resolved(); r.MemoryDurable != e.profile.MemoryDurable || r.Identity == "" {
		t.Fatalf("resolved profile dropped memory criteria: %+v", r)
	}
	if s := e.memoryExtractionSystem(); !strings.Contains(s, "Customer escalation contacts") || !strings.Contains(s, "One-off ticket details") {
		t.Fatalf("extraction prompt: %s", s)
	}
	var got struct {
		Instructions string            `json:"instructions"`
		Criteria     map[string]string `json:"criteria"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.Unmarshal(req.Questions["durable_memory"], &got)
		_, _ = w.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	if _, ok := e.triageMemory(context.Background(), e.cfg.EffectiveJev(), MemoryCandidate{Kind: "fact", Text: "Escalate to Dana"}); !ok {
		t.Fatal("triage failed")
	}
	if got.Instructions != memoryProfileTriageQuestion || got.Criteria["true"] != "Customer escalation contacts" || got.Criteria["false"] != "One-off ticket details" {
		t.Fatalf("question payload: %+v", got)
	}
	e.profile = Profile{}
	if _, ok := e.triageMemory(context.Background(), e.cfg.EffectiveJev(), MemoryCandidate{Kind: "fact", Text: "x"}); !ok {
		t.Fatal("triage failed")
	}
	if got.Instructions != memoryTriageQuestion || got.Criteria["true"] != defaultMemoryDurable || got.Criteria["false"] != defaultMemoryNotDurable {
		t.Fatalf("default question payload: %+v", got)
	}
}

func TestMemoryWakeRetriageKeepUncertainAndRecordsScore(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "discard_uncertain", true: "keep_uncertain"}[keep], func(t *testing.T) {
			ctx := context.Background()
			e := memoryEngine(t, config.MemoryConfig{KeepUncertain: keep})
			prev := memoryTempDir
			memoryTempDir = func() string { return t.TempDir() }
			defer func() { memoryTempDir = prev }()
			real := t.TempDir() + "-real"
			if err := os.Mkdir(real, 0o755); err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(real)
			w, _ := e.ensureMemoryWorkspace(ctx, real)
			good := commitMemory(t, e, w, "good convention", "pending", 0.7)
			unsure := commitMemory(t, e, w, "unsure convention", "pending", 0.7)
			bad := commitMemory(t, e, w, "bad convention", "pending", 0.7)
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				var req struct {
					State MemoryCandidate `json:"state"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				score := map[string]string{"good": "0.9", "unsure": "0.6", "bad": "0.3"}[strings.Fields(req.State.Text)[0]]
				_, _ = rw.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":` + score + `}}}`))
			}))
			defer srv.Close()
			e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
			e.cleanupPendingMemory(ctx)
			recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, IncludeDeleted: true})
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]store.MemoryRecord{}
			for _, r := range recs {
				byID[r.ID] = r
			}
			check := func(id, status string, confidence float64) {
				t.Helper()
				if r := byID[id]; r.Status != status || r.Confidence != confidence {
					t.Fatalf("%q: status=%q confidence=%v, want %q %v", r.Text, r.Status, r.Confidence, status, confidence)
				}
			}
			check(good.ID, "active", 0.9)
			check(bad.ID, "deleted", 0.3)
			if keep {
				check(unsure.ID, "pending", 0.7)
			} else {
				check(unsure.ID, "deleted", 0.6)
			}
		})
	}
}

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/store"
)

func TestMemoryCandidateTriage(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
	}{
		{"reject", `{"answers":{"durable_memory":{"type":"noul","noul":0.49}}}`, 0},
		{"accept", `{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`, 1},
		{"missing", `{"answers":{}}`, 1},
		{"malformed", `not json`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := memoryEngine(t, config.MemoryConfig{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
			root := t.TempDir()
			c := MemoryCandidate{Kind: "command", Text: "Run go test ./...", EvidenceRefs: []string{"go.mod"}}
			if err := e.persistMemoryCandidates(context.Background(), "s1", root, []MemoryCandidate{c}, nil); err != nil {
				t.Fatal(err)
			}
			w, _ := e.ensureMemoryWorkspace(context.Background(), root)
			recs, err := e.store.ListMemory(context.Background(), store.MemoryFilter{WorkspaceID: w.ID})
			if err != nil || len(recs) != tc.want {
				t.Fatalf("records=%+v err=%v", recs, err)
			}
			if len(recs) > 0 && recs[0].Status != "pending" {
				t.Fatalf("triage activated proposal: %+v", recs[0])
			}
		})
	}
}

func TestMemoryCandidatesDistinctSessionsAndCorrection(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{Inject: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	root := t.TempDir()
	candidate := MemoryCandidate{Kind: "fact", Text: "Tests live beside sources", EvidenceRefs: []string{"source.go"}}
	persist := func(sid string) {
		t.Helper()
		if err := e.persistMemoryCandidates(ctx, sid, root, []MemoryCandidate{candidate}, []string{"event:" + sid}); err != nil {
			t.Fatal(err)
		}
	}
	records := func() []store.MemoryRecord {
		t.Helper()
		w, _ := e.ensureMemoryWorkspace(ctx, root)
		recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true})
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}
	persist("s1")
	persist("s1")
	recs := records()
	if len(recs) != 1 || recs[0].Status != "pending" {
		t.Fatalf("same session promoted or duplicated: %+v", recs)
	}
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundarySessionStart, 1, nil)
	block := e.memoryForRequest(e.cfg, "s1")
	if !strings.Contains(block, "New memory proposals") || !strings.Contains(block, recs[0].ID) || !strings.Contains(block, "not established facts") {
		t.Fatalf("missing pending presentation: %s", block)
	}
	persist("s2")
	recs = records()
	if len(recs) != 1 || recs[0].Status != "active" || !strings.Contains(recs[0].EvidenceRefs, "event:s1") || !strings.Contains(recs[0].EvidenceRefs, "event:s2") {
		t.Fatalf("promotion/evidence: %+v", recs)
	}
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundarySessionStart, 2, nil)
	if block = e.memoryForRequest(e.cfg, "s1"); strings.Contains(block, "New memory proposals") || !strings.Contains(block, candidate.Text) {
		t.Fatalf("active injection: %s", block)
	}
	old := recs[0]
	result, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": old.ID, "text": "Tests live under testdata", "user_confirmed": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := result.(store.MemoryRecord)
	persist("s2")
	persist("s1")
	recs = records()
	if len(recs) != 2 {
		t.Fatalf("resurrected correction: %+v", recs)
	}
	for _, rec := range recs {
		if rec.ID == old.ID && (rec.Status != "superseded" || rec.SupersededBy != replacement.ID) {
			t.Fatalf("lost supersession: %+v", rec)
		}
		if rec.Status == "active" && rec.ID != replacement.ID {
			t.Fatalf("old inference active: %+v", rec)
		}
	}
}

func TestMemoryTriageFailureStaysPendingAcrossSessions(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{AutoCommit: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	root := t.TempDir()
	for _, sid := range []string{"s1", "s2"} {
		if err := e.persistMemoryCandidates(ctx, sid, root, []MemoryCandidate{{Kind: "lesson", Text: "Keep tests deterministic"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID})
	if err != nil || len(recs) != 1 || recs[0].Status != "pending" {
		t.Fatalf("failed triage promoted: %+v %v", recs, err)
	}
}

func TestMemoryDuplicateNormalization(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{" Run  GO test ", "run go TEST", true},
		{"Run all repository tests with go test", "Run all repository tests with go test.", true},
		{"use make test", "use go test", false},
	} {
		if got := memoryDuplicate(tc.a, tc.b); got != tc.want {
			t.Errorf("duplicate(%q,%q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestMemoryInjectionDefaultBoundsAndExpiry(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.Default().Memory)
	root := t.TempDir()
	w, ok := e.ensureMemoryWorkspace(ctx, root)
	if !ok {
		t.Fatal("workspace unavailable")
	}
	for i := 0; i < 10; i++ {
		if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: strings.Repeat("repository tests ", 20), Provenance: "observed", Confidence: .8}); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "EXPIRED MEMORY", Provenance: "observed", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundarySessionStart, 1, nil)
	ep, ok := e.pinnedMemory("s1")
	if !ok || len(ep.records) != 8 {
		t.Fatalf("pinned records: %+v", ep)
	}
	tokens := 0
	for _, rec := range ep.records {
		if rec.Status != "active" {
			t.Fatalf("non-active pinned: %+v", rec)
		}
		tokens += estimate(rec.Text)
	}
	if tokens > 1200 {
		t.Fatalf("injection tokens=%d", tokens)
	}
	block := e.memoryForRequest(e.cfg, "s1")
	if block == "" || strings.Contains(block, "EXPIRED MEMORY") {
		t.Fatalf("injection: %s", block)
	}
	if block := e.memoryForRequest(config.Config{Memory: config.MemoryConfig{Inject: false}}, "s1"); block != "" {
		t.Fatal("disabled injection populated context")
	}
	expired, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, Status: "expired"})
	if err != nil || len(expired) != 1 {
		t.Fatalf("expiry was not swept: %+v %v", expired, err)
	}
}

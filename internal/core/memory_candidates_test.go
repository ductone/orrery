package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/store"
)

func TestMemoryCandidateTriage(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
		status         string
	}{
		{"code_description_discarded", `{"answers":{"durable_memory":{"type":"noul","noul":0.49}}}`, 0, ""},
		{"uncertain_stays_pending", `{"answers":{"durable_memory":{"type":"noul","noul":0.6}}}`, 1, "pending"},
		{"workflow_activates", `{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`, 1, "active"},
		{"missing", `{"answers":{}}`, 1, "pending"},
		{"malformed", `not json`, 1, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := memoryEngine(t, config.MemoryConfig{})
			var question string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Questions map[string]struct {
						Instructions string `json:"instructions"`
					} `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if q, ok := req.Questions["durable_memory"]; ok {
					question = q.Instructions
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
			root := t.TempDir()
			c := MemoryCandidate{Kind: "command", Text: "Run go install ./cmd/orrery after pushing", EvidenceRefs: []string{"go.mod"}}
			if err := e.persistMemoryCandidates(context.Background(), "s1", root, []MemoryCandidate{c}, nil); err != nil {
				t.Fatal(err)
			}
			if question != memoryTriageQuestion {
				t.Fatalf("triage question = %q", question)
			}
			w, _ := e.ensureMemoryWorkspace(context.Background(), root)
			recs, err := e.store.ListMemory(context.Background(), store.MemoryFilter{WorkspaceID: w.ID})
			if err != nil || len(recs) != tc.want {
				t.Fatalf("records=%+v err=%v", recs, err)
			}
			if len(recs) > 0 && recs[0].Status != tc.status {
				t.Fatalf("status = %q, want %q: %+v", recs[0].Status, tc.status, recs[0])
			}
			if tc.name == "workflow_activates" && recs[0].Confidence != 0.9 {
				t.Fatalf("triage score not recorded: %+v", recs[0])
			}
		})
	}
}

func TestMemoryCandidatesActivateWithoutConfirmation(t *testing.T) {
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
	persist("s2")
	recs := records()
	if len(recs) != 1 || recs[0].Status != "active" || !strings.Contains(recs[0].EvidenceRefs, "event:s1") || !strings.Contains(recs[0].EvidenceRefs, "event:s2") {
		t.Fatalf("activation/evidence: %+v", recs)
	}
	// A pending record is never presented for confirmation.
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "lesson", Text: "PENDING NOTE", Provenance: "extracted", Confidence: 0.6, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundarySessionStart, 1, nil)
	block := e.memoryForRequest(e.cfg, "s1")
	if !strings.Contains(block, candidate.Text) || strings.Contains(block, "PENDING NOTE") || strings.Contains(block, "proposal") || strings.Contains(block, "confirm") {
		t.Fatalf("injection: %s", block)
	}
	old := recs[0]
	result, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": old.ID, "text": "Tests live under testdata", "user_confirmed": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := result.(store.MemoryRecord)
	persist("s2")
	persist("s1")
	for _, rec := range records() {
		if rec.ID == old.ID && (rec.Status != "superseded" || rec.SupersededBy != replacement.ID) {
			t.Fatalf("lost supersession: %+v", rec)
		}
		if rec.Status == "active" && rec.Text == old.Text {
			t.Fatalf("old inference active: %+v", rec)
		}
	}
}

func TestMemoryCandidateContradictionSupersedes(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	old, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "command", Text: "Run make test to verify", Provenance: "extracted", Confidence: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]any `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req.Questions["conflict_"+old.ID]; ok {
			_, _ = w.Write([]byte(`{"answers":{"conflict_` + old.ID + `":{"type":"noul","noul":0.95}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	if err := e.persistMemoryCandidates(ctx, "s1", root, []MemoryCandidate{{Kind: "command", Text: "Run go test ./... to verify; make test was removed"}}, nil); err != nil {
		t.Fatal(err)
	}
	recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true})
	if err != nil || len(recs) != 2 {
		t.Fatalf("records=%+v err=%v", recs, err)
	}
	for _, rec := range recs {
		if rec.ID == old.ID && (rec.Status != "superseded" || rec.SupersededBy == "") {
			t.Fatalf("contradicted memory not superseded: %+v", rec)
		}
		if rec.ID != old.ID && rec.Status != "active" {
			t.Fatalf("replacement not active: %+v", rec)
		}
	}
	events, err := e.store.EventsAfter(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		found = found || ev.Type == "memory.contradicted"
	}
	if !found {
		t.Fatal("missing memory.contradicted event")
	}
}

func TestMemoryToolContradictedForgetNeedsNoConfirmation(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	rec, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "The module path is orrery", Provenance: "extracted", Confidence: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.controlMemory(ctx, "s1", root, "forget", map[string]any{"id": rec.ID}, nil); err == nil {
		t.Fatal("forget without confirmation or contradiction must fail")
	}
	if _, err := e.controlMemory(ctx, "s1", root, "forget", map[string]any{"id": rec.ID, "contradicted": true}, nil); err != nil {
		t.Fatal(err)
	}
	if recs, _ := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID}); len(recs) != 0 {
		t.Fatalf("contradicted memory kept: %+v", recs)
	}
}

func TestMemoryRetriagePending(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	for _, text := range []string{"Counter in counter.go uses a sync.Mutex", "The module path is spelled orrey", "Maybe useful later"} {
		if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: text, Provenance: "extracted", Confidence: 0.7, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State MemoryCandidate `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		score := "0.6"
		switch {
		case strings.Contains(req.State.Text, "sync.Mutex"):
			score = "0.1"
		case strings.Contains(req.State.Text, "orrey"):
			score = "0.9"
		}
		_, _ = w.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":` + score + `}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	if err := e.persistMemoryCandidates(ctx, "s1", root, nil, nil); err != nil {
		t.Fatal(err)
	}
	recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID})
	if err != nil || len(recs) != 2 {
		t.Fatalf("records=%+v err=%v", recs, err)
	}
	for _, rec := range recs {
		want := "pending"
		if strings.Contains(rec.Text, "orrey") {
			want = "active"
		}
		if rec.Status != want {
			t.Fatalf("%q status=%q want %q", rec.Text, rec.Status, want)
		}
	}
}

func TestMemorySkipsTempWorkspaces(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	tmp := t.TempDir()
	prev := memoryTempDir
	memoryTempDir = func() string { return tmp }
	defer func() { memoryTempDir = prev }()
	root := filepath.Join(tmp, "orrery-benchmark-1")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if !memoryTempWorkspace(root) || memoryTempWorkspace(tmp) || memoryTempWorkspace(t.TempDir()+"-elsewhere") {
		t.Fatal("temp workspace detection")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	if err := e.persistMemoryCandidates(ctx, "s1", root, []MemoryCandidate{{Kind: "command", Text: "Run go test ./..."}}, nil); err != nil {
		t.Fatal(err)
	}
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	if recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true, IncludeDeleted: true}); err != nil || len(recs) != 0 {
		t.Fatalf("benchmark workspace recorded memory: %+v %v", recs, err)
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

func TestMemoryCleanupPending(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	tmp := t.TempDir()
	prev := memoryTempDir
	memoryTempDir = func() string { return tmp }
	defer func() { memoryTempDir = prev }()
	bench := filepath.Join(tmp, "orrery-benchmark-2")
	if err := os.Mkdir(bench, 0o755); err != nil {
		t.Fatal(err)
	}
	real := t.TempDir() + "-real"
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(real)
	bw, _ := e.ensureMemoryWorkspace(ctx, bench)
	rw, _ := e.ensureMemoryWorkspace(ctx, real)
	commit := func(w store.Workspace, text string) {
		t.Helper()
		if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: text, Provenance: "extracted", Confidence: 0.7, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	commit(bw, "Counter uses a mutex")
	commit(rw, "The module path is spelled orrey")
	commit(rw, "Maybe useful later")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State MemoryCandidate `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		score := "0.6"
		if strings.Contains(req.State.Text, "orrey") {
			score = "0.9"
		}
		_, _ = w.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":` + score + `}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	e.cleanupPendingMemory(ctx)
	if recs, _ := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: bw.ID}); len(recs) != 0 {
		t.Fatalf("benchmark pending kept: %+v", recs)
	}
	recs, _ := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: rw.ID})
	if len(recs) != 1 || recs[0].Status != "active" || !strings.Contains(recs[0].Text, "orrey") {
		t.Fatalf("cleanup result: %+v", recs)
	}
}

func TestMemoryRetriageRunsConcurrentlyAndBounded(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	for i := 0; i < 12; i++ {
		if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: fmt.Sprintf("Convention number %d for this repo", i), Provenance: "extracted", Confidence: 0.7, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	prev := memoryTriageConcurrency
	memoryTriageConcurrency = 4
	defer func() { memoryTriageConcurrency = prev }()
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		_, _ = rw.Write([]byte(`{"answers":{"durable_memory":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	e.cfg.Jev.APIKey, e.cfg.Jev.BaseURL = "key", srv.URL
	e.retriagePendingMemory(ctx, "s1", w, e.cfg.EffectiveJev(), false)
	if p := peak.Load(); p < 2 || p > 4 {
		t.Fatalf("peak concurrent calls = %d, want 2..4", p)
	}
	recs, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, Status: "active"})
	if err != nil || len(recs) != 12 {
		t.Fatalf("active=%d err=%v", len(recs), err)
	}
}

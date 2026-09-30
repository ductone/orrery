package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

// reviewHarness runs reviewWorkspace against a real git workspace, a stub
// reviewer model, and (optionally) a stub Jev.
type reviewHarness struct {
	t         *testing.T
	e         *Engine
	st        *store.Store
	workspace string
	sid       string

	mu       sync.Mutex
	specs    []string // reviewer specs, in spawn order
	reviewer func(spec string, attempt int) map[string]any
	attempts map[string]int

	jevCalls atomic.Int32
	jev      func(state map[string]any, question string) float64
}

func newReviewHarness(t *testing.T, withJev bool) *reviewHarness {
	t.Helper()
	h := &reviewHarness{t: t, attempts: map[string]int{}}
	h.e, h.st = testEngine(t)
	h.workspace = t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", h.workspace, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	h.write("README.md", "# Project\n")
	git("add", "-A")
	git("commit", "-qm", "base")

	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		instructions, _ := body["instructions"].(string)
		if !strings.Contains(instructions, "Review this proposed workspace diff") {
			_ = json.NewEncoder(w).Encode(responsesText("Title"))
			return
		}
		h.mu.Lock()
		if h.attempts[instructions] == 0 {
			h.specs = append(h.specs, instructions)
		}
		h.attempts[instructions]++
		attempt := h.attempts[instructions]
		h.mu.Unlock()
		reply := h.reviewer(instructions, attempt)
		if status, ok := reply["__status"].(int); ok {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"credits_reserved"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(models.Close)
	cfg := config.Config{
		WorkspaceRoot: h.workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: models.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
		Budget:        config.BudgetConfig{SessionUSD: 50, JobDefaultFraction: 0.2},
		Interventions: config.InterventionConfig{JudgeEnabled: new(bool)},
	}
	if withJev {
		jevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.jevCalls.Add(1)
			var req struct {
				State     map[string]any             `json:"state"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			answers := map[string]any{}
			for name := range req.Questions {
				if name == "risk" {
					answers[name] = map[string]any{"type": "score", "score": 0.0, "legend": map[string]string{"0": "l", "1": "m", "2": "h"}}
					continue
				}
				answers[name] = map[string]any{"type": "noul", "noul": h.jev(req.State, name)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers})
		}))
		t.Cleanup(jevServer.Close)
		cfg.Jev = config.JevConfig{APIKey: "k", BaseURL: jevServer.URL, Review: true}
	}
	h.e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	h.sid = uuid.NewString()
	if err := h.st.CreateSession(context.Background(), store.Session{ID: h.sid, Spec: "Add the feature", Phase: "review", BudgetUSD: 50, WorkspacePath: h.workspace}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *reviewHarness) write(rel, body string) {
	h.t.Helper()
	p := filepath.Join(h.workspace, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *reviewHarness) run() (bool, string, error) {
	h.t.Helper()
	req := agentproto.TaskRequest{
		Spec:      "Add the feature",
		Budget:    agentproto.Budget{MaxUSD: 50, MaxTokens: 10_000_000, MaxWallClock: time.Minute, MaxDepth: 2},
		Workspace: agentproto.Workspace{Path: h.workspace, Mode: "shared-write", Ownership: "external"},
		Depth:     1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return h.e.reviewWorkspace(ctx, h.sid, "", req, nil)
}

func (h *reviewHarness) events(typ string) []map[string]any {
	h.t.Helper()
	es, err := h.st.EventsAfter(context.Background(), h.sid, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range es {
		if e.Type == typ {
			var m map[string]any
			_ = json.Unmarshal(e.Data, &m)
			out = append(out, m)
		}
	}
	return out
}

func verdictJSON(pass bool, findings ...string) map[string]any {
	if findings == nil {
		findings = []string{}
	}
	b, _ := json.Marshal(map[string]any{"pass": pass, "findings": findings})
	return responsesText(string(b))
}

func emptyReply() map[string]any {
	return map[string]any{"model": "gpt-5.6-terra", "status": "completed", "output": []any{map[string]any{"type": "reasoning", "summary": []any{}}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 50}}
}

// stateFile is the file a Jev per-file question is about, or "".
func stateFile(state map[string]any) string {
	f, _ := state["file"].(string)
	return f
}

func TestReviewSkipsWhenOnlyProseAndAssetsChanged(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("docs/guide.md", "# Guide\n\nLots of prose.\n")
	h.write("logo.svg", "<svg></svg>\n")
	h.write("README.md", "# Project\n\nMore words.\n")
	h.jev = func(map[string]any, string) float64 { return 0.05 }
	h.reviewer = func(string, int) map[string]any { t.Fatal("no reviewer should run"); return nil }
	passed, text, err := h.run()
	if err != nil || !passed || !strings.Contains(text, "no reviewable changes") {
		t.Fatalf("passed=%v text=%s err=%v", passed, text, err)
	}
	plans := h.events("review.plan")
	if len(plans) != 1 || plans[0]["skip"] != true {
		t.Fatalf("plan = %v", plans)
	}
}

func TestReviewShowsCodeAndListsTriagedFiles(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Feature() int { return 1 }\n")
	h.write("docs/rfc.md", "# RFC\n\n"+strings.Repeat("Discussion.\n", 200))
	h.write("ci/pipeline.yaml", "steps:\n  - run: make test\n")
	h.write("assets/logo.svg", strings.Repeat("<path/>\n", 500))
	h.jev = func(state map[string]any, q string) float64 {
		switch {
		case q == "needs_review" && stateFile(state) == "ci/pipeline.yaml":
			return 0.9
		case q == "needs_review":
			return 0.05
		}
		return 0.1
	}
	h.reviewer = func(string, int) map[string]any { return verdictJSON(true) }
	passed, _, err := h.run()
	if err != nil || !passed {
		t.Fatalf("passed=%v err=%v", passed, err)
	}
	if len(h.specs) != 1 {
		t.Fatalf("reviewers = %d", len(h.specs))
	}
	spec := h.specs[0]
	for _, want := range []string{"diff --git a/internal/feature.go", "diff --git a/ci/pipeline.yaml", "ALSO CHANGED, NOT SHOWN", "docs/rfc.md", "assets/logo.svg"} {
		if !strings.Contains(spec, want) {
			t.Errorf("reviewer spec missing %q", want)
		}
	}
	for _, unwanted := range []string{"diff --git a/docs/rfc.md", "diff --git a/assets/logo.svg", "Discussion."} {
		if strings.Contains(spec, unwanted) {
			t.Errorf("reviewer spec must not include %q", unwanted)
		}
	}
	// The reviewer's turn limit comes from the plan.
	jobs, _ := h.st.Jobs(context.Background(), h.sid)
	var hints agentproto.RoutingHints
	_ = json.Unmarshal([]byte(jobs[0].HintsJSON), &hints)
	if hints.WorkerTurns < 6 || !hints.Review {
		t.Fatalf("hints = %+v", hints)
	}
}

func TestReviewWithoutJevReviewsEverythingButAssets(t *testing.T) {
	h := newReviewHarness(t, false)
	h.write("internal/feature.go", "package internal\n")
	h.write("docs/rfc.md", "# RFC\n")
	h.write("logo.png", "\x89PNG\x00\x01")
	h.reviewer = func(string, int) map[string]any { return verdictJSON(true) }
	if passed, _, err := h.run(); err != nil || !passed {
		t.Fatalf("passed=%v err=%v", passed, err)
	}
	if !strings.Contains(h.specs[0], "diff --git a/docs/rfc.md") || strings.Contains(h.specs[0], "diff --git a/logo.png") {
		t.Fatal("without Jev, prose is reviewed and binaries are listed")
	}
	if h.jevCalls.Load() != 0 {
		t.Fatal("no Jev calls without jev.review")
	}
}

func TestReviewDowngradesFindingsThatAreNotBugs(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Feature(p *int) int { return *p }\n")
	h.jev = func(state map[string]any, q string) float64 {
		if q == "real_bug" {
			if strings.Contains(fmt.Sprint(state["finding"]), "nil") {
				return 0.9
			}
			return 0.05
		}
		return 0.3
	}
	h.reviewer = func(string, int) map[string]any {
		return verdictJSON(false, "feature.go: Feature dereferences p without a nil check", "consider renaming p to ptr")
	}
	passed, text, err := h.run()
	if err != nil || passed {
		t.Fatalf("a real finding still fails the review: passed=%v err=%v", passed, err)
	}
	var out struct {
		Findings []string `json:"findings"`
		Notes    []string `json:"notes"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	if len(out.Findings) != 1 || !strings.Contains(out.Findings[0], "nil check") || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "renaming") {
		t.Fatalf("review = %s", text)
	}
}

func TestReviewPassesWhenEveryFindingIsNoise(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n")
	h.jev = func(_ map[string]any, q string) float64 {
		if q == "real_bug" {
			return 0.02
		}
		return 0.3
	}
	h.reviewer = func(string, int) map[string]any { return verdictJSON(false, "add a doc comment") }
	passed, text, err := h.run()
	if err != nil || !passed || !strings.Contains(text, "downgraded finding") {
		t.Fatalf("passed=%v text=%s err=%v", passed, text, err)
	}
}

func TestInconclusiveLowRiskReviewIsAcceptedWithoutARerun(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nconst Name = \"feature\"\n")
	h.jev = func(map[string]any, string) float64 { return 0.05 }
	h.reviewer = func(string, int) map[string]any { return emptyReply() }
	passed, text, err := h.run()
	if err != nil || !passed || !strings.Contains(text, "accepted as low risk") {
		t.Fatalf("passed=%v text=%s err=%v", passed, text, err)
	}
	if jobs, _ := h.st.Jobs(context.Background(), h.sid); len(jobs) != 1 {
		t.Fatalf("an accepted part must not be reviewed again: %d reviewers", len(jobs))
	}
}

func TestInconclusiveRiskyReviewIsRerunWithMoreRoom(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Transfer() {}\n")
	h.jev = func(map[string]any, string) float64 { return 0.8 }
	first := true
	h.reviewer = func(spec string, attempt int) map[string]any {
		// The first reviewer never answers; the retry does.
		h.mu.Lock()
		defer h.mu.Unlock()
		if first && attempt <= 3 {
			if attempt == 3 {
				first = false
			}
			return emptyReply()
		}
		return verdictJSON(true)
	}
	passed, _, err := h.run()
	if err != nil || !passed {
		t.Fatalf("passed=%v err=%v", passed, err)
	}
	jobs, _ := h.st.Jobs(context.Background(), h.sid)
	if len(jobs) != 2 {
		t.Fatalf("reviewers = %d, want a retry", len(jobs))
	}
	var firstHints, retryHints agentproto.RoutingHints
	_ = json.Unmarshal([]byte(jobs[0].HintsJSON), &firstHints)
	_ = json.Unmarshal([]byte(jobs[1].HintsJSON), &retryHints)
	if retryHints.WorkerTurns <= firstHints.WorkerTurns {
		t.Fatalf("the retry must get more room: %d -> %d", firstHints.WorkerTurns, retryHints.WorkerTurns)
	}
}

func TestInconclusiveWithoutJevIsReportedAfterOneRetry(t *testing.T) {
	h := newReviewHarness(t, false)
	h.write("internal/feature.go", "package internal\n")
	h.reviewer = func(string, int) map[string]any { return emptyReply() }
	_, _, err := h.run()
	if err == nil || !strings.Contains(err.Error(), "review inconclusive") {
		t.Fatalf("err = %v", err)
	}
	if jobs, _ := h.st.Jobs(context.Background(), h.sid); len(jobs) != 2 {
		t.Fatalf("reviewers = %d, want the original and one retry", len(jobs))
	}
}

func TestLargeReviewsAreShardedAndMerged(t *testing.T) {
	h := newReviewHarness(t, false)
	for _, dir := range []string{"alpha", "beta", "gamma"} {
		h.write(dir+"/big.go", "package "+dir+"\n"+strings.Repeat("// line of code in "+dir+"\n", 2_500))
	}
	h.reviewer = func(spec string, _ int) map[string]any {
		if strings.Contains(spec, "diff --git a/beta/big.go") {
			return verdictJSON(false, "beta/big.go: off-by-one")
		}
		return verdictJSON(true)
	}
	passed, text, err := h.run()
	if err != nil || passed {
		t.Fatalf("passed=%v err=%v", passed, err)
	}
	if len(h.specs) < 2 {
		t.Fatalf("a large diff must be split across reviewers, got %d", len(h.specs))
	}
	if !strings.Contains(text, "off-by-one") || !strings.Contains(text, "[part ") {
		t.Fatalf("review = %s", text)
	}
	for _, spec := range h.specs {
		if !strings.Contains(spec, " of ") {
			t.Fatal("each shard's spec must say which part it is")
		}
	}
}

// A reviewer whose only credential is rate limited waits for it instead of
// failing the review, as a transient 429 once did.
func TestReviewerWaitsOutARateLimit(t *testing.T) {
	h := newReviewHarness(t, false)
	// The credential rests longer than the engine's retry pause, as a real
	// 30-second backoff outlasts the 5-second one.
	restore := retryDelay
	retryDelay = func(int) time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() { retryDelay = restore })
	h.write("internal/feature.go", "package internal\n")
	h.reviewer = func(_ string, attempt int) map[string]any {
		if attempt == 1 {
			return map[string]any{"__status": 429}
		}
		return verdictJSON(true)
	}
	passed, _, err := h.run()
	if err != nil || !passed {
		t.Fatalf("passed=%v err=%v", passed, err)
	}
	if jobs, _ := h.st.Jobs(context.Background(), h.sid); len(jobs) != 1 {
		t.Fatalf("the first reviewer must finish; got %d reviewers", len(jobs))
	}
}

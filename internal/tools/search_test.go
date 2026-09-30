package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/jev"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func repeat(line string, n int) string { return strings.Repeat(line+"\n", n) }

// stubRanker scores files by substring and records what it was asked.
type stubRanker struct {
	scores map[string]float64
	err    error
	asked  []RankFile
}

func (s *stubRanker) Rank(_ context.Context, _ string, files []RankFile) ([]float64, error) {
	s.asked = files
	if s.err != nil {
		return nil, s.err
	}
	out := make([]float64, len(files))
	for i, f := range files {
		for sub, score := range s.scores {
			if strings.Contains(f.Path, sub) {
				out[i] = score
			}
		}
	}
	return out, nil
}

func TestTruncatedSearchReportsTotals(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.go": repeat("token", 5), "b.go": repeat("token", 12)})
	r := New(root)
	v, err := r.Call(context.Background(), "search", map[string]any{"pattern": "token", "max_results": float64(4)})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("a truncated search must report totals, got %T", v)
	}
	if len(out["matches"].([]map[string]any)) != 4 || out["total_matches"] != 17 || out["files_with_matches"] != 2 {
		t.Fatalf("out = %v", out)
	}
	top := out["top_files"].([]map[string]any)
	if top[0]["path"] != "b.go" || top[0]["match_count"] != 12 {
		t.Fatalf("top files = %v", top)
	}
	if strings.Contains(out["hint"].(string), "intent") {
		t.Fatal("without a ranker the hint must not offer intent")
	}
	if got := SearchResultPaths(v); len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("paths = %v", got)
	}
}

func TestSearchSchemaUnchangedWithoutRanker(t *testing.T) {
	r := New(t.TempDir())
	for _, d := range r.Definitions() {
		if d.Name == "search" {
			if _, ok := d.InputSchema["properties"].(map[string]any)["intent"]; ok || d.Description != searchDescription {
				t.Fatal("search must keep its original definition without a ranker")
			}
		}
	}
	r.EnableSearchRanking(&stubRanker{})
	for _, d := range r.Definitions() {
		if d.Name == "search" {
			if _, ok := d.InputSchema["properties"].(map[string]any)["intent"]; !ok {
				t.Fatal("ranking must add intent")
			}
		}
	}
}

func TestRankedSearchOrdersFilesAndListsTheRest(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"auth/refresh.go":    repeat("token", 3),
		"docs/tokens.md":     repeat("token", 10),
		"testdata/fixture.x": repeat("token", 10),
		"lexer/lex.go":       repeat("token", 10),
		"misc/other.go":      repeat("token", 2),
	})
	ranker := &stubRanker{scores: map[string]float64{"auth": .95, "docs": .5, "lexer": .1, "testdata": .05, "misc": .02}}
	r := New(root)
	r.EnableSearchRanking(ranker)
	v, err := r.Call(context.Background(), "search", map[string]any{"pattern": "token", "intent": "where auth tokens get refreshed"})
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	files := out["files"].([]map[string]any)
	// auth and docs clear the cutoff; lexer is kept only by the minimum shown.
	if len(files) != 3 || files[0]["path"] != filepath.Join("auth", "refresh.go") || files[1]["path"] != filepath.Join("docs", "tokens.md") || files[2]["path"] != filepath.Join("lexer", "lex.go") {
		t.Fatalf("files = %v", files)
	}
	if n := len(files[0]["matches"].([]map[string]any)); n != 3 {
		t.Fatalf("top file lines = %d", n)
	}
	below := out["below_cutoff"].([]map[string]any)
	if out["below_cutoff_count"] != 2 || below[0]["matches"] != nil || below[0]["relevance"] != 0.05 {
		t.Fatalf("below = %v", below)
	}
	if out["total_matches"] != 35 || !strings.Contains(out["hint"].(string), "below the cutoff") {
		t.Fatalf("out = %v", out)
	}
	if len(ranker.asked) != 5 || len(ranker.asked[0].Matches) == 0 || !strings.Contains(ranker.asked[0].Matches[0], "> 1: token") {
		t.Fatalf("ranker saw %+v", ranker.asked)
	}
	if got := SearchResultPaths(v); got[0] != filepath.Join("auth", "refresh.go") {
		t.Fatalf("paths = %v", got)
	}
}

func TestRankedSearchSkipsNarrowResults(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.go": repeat("token", 3), "b.go": repeat("token", 3)})
	ranker := &stubRanker{}
	r := New(root)
	r.EnableSearchRanking(ranker)
	v, _ := r.Call(context.Background(), "search", map[string]any{"pattern": "token", "intent": "x"})
	if _, ok := v.([]map[string]any); !ok || ranker.asked != nil {
		t.Fatalf("six matches fit on one page and must not be ranked: %T", v)
	}
}

func TestRankedSearchFallsBackOnRankerFailure(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.go": repeat("token", 15), "b.go": repeat("token", 15)})
	r := New(root)
	r.EnableSearchRanking(&stubRanker{err: errors.New("jev: HTTP 529")})
	v, err := r.Call(context.Background(), "search", map[string]any{"pattern": "token", "intent": "x"})
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	if len(out["matches"].([]map[string]any)) != 30 || !strings.Contains(out["ranking_error"].(string), "529") {
		t.Fatalf("out = %v", out)
	}
}

func TestRankedSearchReportsUnrankedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := range searchRankMaxFiles + 5 {
		files[fmt.Sprintf("f%03d.go", i)] = "token\n"
	}
	writeFiles(t, root, files)
	ranker := &stubRanker{scores: map[string]float64{"f": .9}}
	r := New(root)
	r.EnableSearchRanking(ranker)
	v, _ := r.Call(context.Background(), "search", map[string]any{"pattern": "token", "intent": "x"})
	out := v.(map[string]any)
	if len(ranker.asked) != searchRankMaxFiles || out["unranked_count"] != 5 {
		t.Fatalf("asked=%d unranked=%v", len(ranker.asked), out["unranked_count"])
	}
	shown := 0
	for _, f := range out["files"].([]map[string]any) {
		shown += len(f["matches"].([]map[string]any))
	}
	if shown != searchDefaultResults {
		t.Fatalf("shown lines = %d, want the %d-line budget", shown, searchDefaultResults)
	}
}

func TestRankSnippetsSpanTheFile(t *testing.T) {
	if got := spread([]int{0, 1, 2}, 10); len(got) != 3 {
		t.Fatalf("short input must be kept whole: %v", got)
	}
	xs := make([]int, 34)
	for i := range xs {
		xs[i] = i * 10
	}
	got := spread(xs, 10)
	if len(got) != 10 || got[0] != 0 || got[9] != 330 {
		t.Fatalf("spread = %v", got)
	}
}

func TestJevRankerAsksOneNoulPerFile(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			State struct {
				LookingFor string `json:"looking_for"`
				File       string `json:"file"`
			} `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := 0.1
		if req.State.File == "auth.go" && req.State.LookingFor == "refresh" {
			p = 0.9
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"relevant": map[string]any{"type": "noul", "noul": p}}})
	}))
	defer server.Close()
	ranker := JevRanker{Client: jev.New("k", server.URL, "", time.Second), Concurrency: 4}
	scores, err := ranker.Rank(context.Background(), "refresh", []RankFile{{Path: "lexer.go"}, {Path: "auth.go"}, {Path: "x.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || scores[0] != 0.1 || scores[1] != 0.9 {
		t.Fatalf("calls=%d scores=%v", calls.Load(), scores)
	}
}

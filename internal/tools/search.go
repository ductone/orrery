package tools

import (
	"cmp"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ductone/orrey/internal/classify"
	"github.com/ductone/orrey/internal/jev"
)

const (
	searchDefaultResults = 200
	// searchCountCap bounds how far a truncated search keeps scanning to report
	// totals. Past it the total is a lower bound.
	searchCountCap = 20_000
	// Ranking only pays for itself when a pattern matched broadly.
	searchRankMinMatches = 20
	// searchRankMaxFiles bounds ranking latency; further files are reported as
	// unranked rather than silently dropped.
	searchRankMaxFiles = 200
	// searchRankSnippets and searchRankContext shape what the ranker sees of
	// each file: matches sampled evenly across the file, each with surrounding
	// lines. A file's first matches are often imports or incidental mentions.
	searchRankSnippets  = 10
	searchRankContext   = 2
	searchRankLineChars = 200
	// searchRankCutoff is the relevance below which a file is listed without
	// its lines. At least searchRankMinShown files always keep their lines.
	searchRankCutoff    = 0.3
	searchRankMinShown  = 3
	searchRankPerFile   = 20
	searchRankListLimit = 30
	searchRankTimeout   = 10 * time.Second
)

const searchDescription = "Regex search file contents with optional glob."

const rankedSearchDescription = "Regex search file contents with optional glob. When a pattern may match broadly, also pass intent: a short description of what you are looking for. Matching files are then ranked by relevance to it, and low-relevance files are listed by path without their lines."

func searchSchema(ranked bool) map[string]any {
	props := map[string]any{"pattern": str(), "glob": str(), "max_results": num()}
	if ranked {
		props["intent"] = str()
	}
	return schema(props, "pattern")
}

// SearchRanker scores how relevant each file's matches are to what the agent is
// looking for. Scores are independent probabilities in [0,1], comparable across
// files and calls.
type SearchRanker interface {
	Rank(ctx context.Context, intent string, files []RankFile) ([]float64, error)
}

// RankFile is what a ranker sees of one matching file.
type RankFile struct {
	Path    string   `json:"file"`
	Matches []string `json:"matches"`
}

// EnableSearchRanking adds the intent parameter to search. Registries without
// a ranker keep the original definition, so their stable prefix is unchanged.
func (r *Registry) EnableSearchRanking(ranker SearchRanker) {
	if ranker == nil {
		return
	}
	r.ranker = ranker
	for i := range r.defs {
		if r.defs[i].Name == "search" {
			r.defs[i].Description = rankedSearchDescription
			r.defs[i].InputSchema = searchSchema(true)
		}
	}
}

type searchMatch struct {
	path, text string
	line       int
}

type searchFile struct {
	path     string
	count    int
	snippets []string
	// lines holds the file's first matches, kept only when ranking.
	lines []map[string]any
}

type searchScan struct {
	matches []searchMatch
	files   []*searchFile
	total   int
	capped  bool
}

// scan walks the workspace. It keeps the first keep matches in walk order and
// counts every match up to searchCountCap. When ranking, it also keeps each
// file's first matching lines, and snippets of them with surrounding context
// for the ranker.
func (r *Registry) scan(ctx context.Context, re *regexp.Regexp, glob string, keep int, ranking bool) (searchScan, error) {
	var out searchScan
	err := filepath.WalkDir(r.root, func(p string, d fs.DirEntry, e error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e != nil {
			return nil
		}
		if d.IsDir() {
			if ignoredSearchDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(r.root, p)
		if glob != "" && !globMatch(glob, filepath.ToSlash(rel)) {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil || len(b) > 4<<20 {
			return nil
		}
		lines := strings.Split(string(b), "\n")
		var file *searchFile
		var matched []int
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			if file == nil {
				file = &searchFile{path: rel}
				out.files = append(out.files, file)
			}
			file.count++
			out.total++
			if len(out.matches) < keep {
				out.matches = append(out.matches, searchMatch{path: rel, line: i + 1, text: line})
			}
			if ranking {
				matched = append(matched, i)
				if len(file.lines) < searchRankPerFile {
					file.lines = append(file.lines, map[string]any{"line": i + 1, "text": line})
				}
			}
			if out.total >= searchCountCap {
				out.capped = true
				break
			}
		}
		if file != nil && ranking {
			for _, i := range spread(matched, searchRankSnippets) {
				file.snippets = append(file.snippets, snippet(lines, i))
			}
		}
		if out.capped {
			return fs.SkipAll
		}
		return nil
	})
	return out, err
}

// spread picks up to n values evenly across xs, keeping the first and last.
func spread(xs []int, n int) []int {
	if len(xs) <= n {
		return xs
	}
	out := make([]int, n)
	for k := range n {
		out[k] = xs[k*(len(xs)-1)/(n-1)]
	}
	return out
}

func snippet(lines []string, i int) string {
	lo, hi := max(0, i-searchRankContext), min(len(lines), i+searchRankContext+1)
	parts := make([]string, 0, hi-lo)
	for j := lo; j < hi; j++ {
		text := lines[j]
		if len(text) > searchRankLineChars {
			text = text[:searchRankLineChars] + "…"
		}
		marker := "  "
		if j == i {
			marker = "> "
		}
		parts = append(parts, marker+strconv.Itoa(j+1)+": "+text)
	}
	return strings.Join(parts, "\n")
}

func (r *Registry) search(ctx context.Context, a map[string]any) (any, error) {
	re, err := regexp.Compile(asString(a["pattern"]))
	if err != nil {
		return nil, err
	}
	glob := asString(a["glob"])
	maxResults := asInt(a["max_results"], searchDefaultResults)
	intent := strings.TrimSpace(asString(a["intent"]))
	ranked := intent != "" && r.ranker != nil
	s, err := r.scan(ctx, re, glob, maxResults, ranked)
	if err != nil {
		return nil, err
	}
	var rankErr error
	if ranked && s.total > searchRankMinMatches && len(s.files) > 1 {
		result, err := r.rankedResult(ctx, intent, s, maxResults)
		if err == nil {
			return result, nil
		}
		rankErr = err
	}
	rows := make([]map[string]any, 0, len(s.matches))
	for _, m := range s.matches {
		rows = append(rows, map[string]any{"path": m.path, "line": m.line, "text": m.text})
	}
	if s.total <= len(rows) && rankErr == nil {
		return rows, nil
	}
	out := map[string]any{"matches": rows, "total_matches": s.total, "files_with_matches": len(s.files)}
	if s.capped {
		out["total_is_lower_bound"] = true
	}
	if s.total > len(rows) {
		out["top_files"] = topFiles(s.files, 10)
		hint := "Showing the first " + strconv.Itoa(len(rows)) + " of " + strconv.Itoa(s.total) + " matches in walk order. Narrow the pattern or glob"
		if r.ranker != nil {
			hint += ", or pass intent to rank files by relevance"
		}
		out["hint"] = hint + "."
	}
	if rankErr != nil {
		out["ranking_error"] = rankErr.Error()
	}
	return out, nil
}

func (r *Registry) rankedResult(ctx context.Context, intent string, s searchScan, maxResults int) (map[string]any, error) {
	candidates := s.files[:min(len(s.files), searchRankMaxFiles)]
	items := make([]RankFile, len(candidates))
	for i, f := range candidates {
		items[i] = RankFile{Path: filepath.ToSlash(f.path), Matches: f.snippets}
	}
	rankCtx, cancel := context.WithTimeout(ctx, searchRankTimeout)
	defer cancel()
	scores, err := r.ranker.Rank(rankCtx, intent, items)
	if err != nil {
		return nil, err
	}
	order := make([]int, len(candidates))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(scores[b], scores[a]) })

	var files, below []map[string]any
	budget := maxResults
	for rank, i := range order {
		f := candidates[i]
		entry := map[string]any{"path": f.path, "relevance": round2(scores[i]), "match_count": f.count}
		keep := rank < searchRankMinShown || (scores[i] >= searchRankCutoff && budget > 0)
		if !keep {
			below = append(below, entry)
			continue
		}
		lines := f.lines[:min(len(f.lines), max(budget, 1))]
		budget -= len(lines)
		entry["matches"] = lines
		files = append(files, entry)
	}
	out := map[string]any{"intent": intent, "ranked": true, "files": files, "total_matches": s.total, "files_with_matches": len(s.files)}
	if s.capped {
		out["total_is_lower_bound"] = true
	}
	hint := "Files are ordered by relevance to intent."
	if len(below) > 0 {
		out["below_cutoff_count"] = len(below)
		if len(below) > searchRankListLimit {
			below = below[:searchRankListLimit]
		}
		out["below_cutoff"] = below
		hint += " Files below the cutoff are listed without their lines; read one directly, or search again without intent, if it looks relevant."
	}
	if unranked := s.files[len(candidates):]; len(unranked) > 0 {
		out["unranked_count"] = len(unranked)
		out["unranked"] = topFiles(unranked, searchRankListLimit)
		hint += " Too many files matched to rank them all; narrow the pattern or glob to rank the rest."
	}
	out["hint"] = hint
	return out, nil
}

func topFiles(files []*searchFile, n int) []map[string]any {
	sorted := slices.Clone(files)
	slices.SortStableFunc(sorted, func(a, b *searchFile) int { return cmp.Compare(b.count, a.count) })
	out := make([]map[string]any, 0, min(n, len(sorted)))
	for _, f := range sorted[:min(n, len(sorted))] {
		out = append(out, map[string]any{"path": f.path, "match_count": f.count})
	}
	return out
}

func round2(v float64) float64 { return float64(int(v*100+.5)) / 100 }

// SearchResultPaths returns the file paths a search result refers to, in
// result order, for any of the shapes search returns.
func SearchResultPaths(value any) []string {
	var rows []map[string]any
	switch v := value.(type) {
	case []map[string]any:
		rows = v
	case map[string]any:
		if m, ok := v["matches"].([]map[string]any); ok {
			rows = m
		}
		if f, ok := v["files"].([]map[string]any); ok {
			rows = f
		}
	}
	paths := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		p, _ := row["path"].(string)
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}

// JevRanker ranks search results with one Jev noul per file, the pattern
// TypeSafe documents for re-ranking: each call's state is the query paired with
// one candidate, so scores are comparable across files.
type JevRanker struct {
	Client      classify.Classifier
	Concurrency int
}

// SearchRankVersion identifies the ranking question. Bump it when the wording
// or criteria change.
const SearchRankVersion = "search_rank/v1"

var searchRankQuestion = map[string]jev.Question{
	"relevant": jev.Noul(
		"A coding agent searched a repository and is looking for what is described in looking_for. Does this file contain it?",
		"The matching lines define, implement, configure, call, or test what the agent is looking for.",
		"The matches only share a word or a nearby topic, or are unrelated to what the agent is looking for.",
	),
}

func (j JevRanker) Rank(ctx context.Context, intent string, files []RankFile) ([]float64, error) {
	scores := make([]float64, len(files))
	errs := make([]error, len(files))
	sem := make(chan struct{}, max(1, j.Concurrency))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			resp, err := j.Client.Ask(ctx, map[string]any{"looking_for": intent, "file": f.Path, "matches": f.Matches}, searchRankQuestion)
			if err != nil {
				errs[i] = err
				return
			}
			if a := resp.Answers["relevant"]; a.Noul != nil {
				scores[i] = *a.Noul
			}
		}()
	}
	wg.Wait()
	// A partial ranking would demote every file whose call failed, so any
	// failure falls back to the unranked result.
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return scores, nil
}

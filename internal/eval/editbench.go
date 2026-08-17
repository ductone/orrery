package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ductone/orrey/internal/hashline"
	"github.com/ductone/orrey/internal/tools"
)

const (
	legacyDialect     = "hashline-json"
	contextualDialect = "hashline-contextual"
	textDialect       = "text-anchor"
)

// SyntheticEditDialectMetrics is a deterministic, JSON-friendly summary of
// edit behavior. The benchmark deliberately uses the production applier and
// tool registry rather than a model, so model runs can be compared against a
// stable capability baseline.
type SyntheticEditDialectMetrics struct {
	CaseCount                 int     `json:"case_count"`
	FirstAttemptOpportunities int     `json:"first_attempt_opportunities"`
	FirstAttemptSuccesses     int     `json:"first_attempt_successes"`
	FirstAttemptRate          float64 `json:"first_attempt_rate"`
	AmbiguitiesDetected       int     `json:"ambiguities_detected"`
	ContextStalenessDetected  int     `json:"context_staleness_detected"`
	EscalationsDetected       int     `json:"escalations_detected"`
	Retries                   int     `json:"retries"`
	ToolCallProxyCount        int     `json:"tool_call_proxy_count"`
	NoopLoopsDetected         int     `json:"no_op_loops_detected"`
	ConflictsDetected         int     `json:"conflicts_detected"`
	LargeFileRecovery         bool    `json:"large_file_recovery"`
}

// SyntheticEditBenchmark is the complete result, keyed by the exact dialect
// strings accepted by tools.NewWithStateDialect.
type SyntheticEditBenchmark struct {
	Dialects map[string]SyntheticEditDialectMetrics `json:"dialects"`
}

// RunSyntheticEditBenchmark exercises unique and repeated anchors, contextual
// staleness, the error ladder, no-op protection, large-file reads, and
// optimistic locking.
func RunSyntheticEditBenchmark() SyntheticEditBenchmark {
	result := SyntheticEditBenchmark{Dialects: make(map[string]SyntheticEditDialectMetrics, 3)}
	for _, dialect := range []string{legacyDialect, contextualDialect, textDialect} {
		result.Dialects[dialect] = runSyntheticDialect(dialect)
	}
	return result
}

func runSyntheticDialect(dialect string) SyntheticEditDialectMetrics {
	root, err := os.MkdirTemp("", "orrery-editbench-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	ctx := context.Background()
	m := SyntheticEditDialectMetrics{CaseCount: 7, FirstAttemptOpportunities: 2}
	mode := benchmarkAnchorMode(dialect)

	write := func(name string, lines []string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			panic(err)
		}
		return path
	}
	read := func(registry *tools.Registry, name string, args map[string]any) any {
		m.ToolCallProxyCount++
		args["path"] = name
		value, err := registry.Call(ctx, "read", args)
		if err != nil {
			panic(err)
		}
		return value
	}
	edit := func(registry *tools.Registry, name, anchor string, insert []string) (any, error) {
		m.ToolCallProxyCount++
		return registry.Call(ctx, "edit", map[string]any{
			"path": name,
			"hunks": []any{map[string]any{
				"anchor": anchor,
				"delete": 1,
				"insert": insert,
			}},
		})
	}

	// 1: a unique line is editable on the first attempt in every dialect.
	write("unique.txt", []string{"alpha", "beta", "gamma"})
	uniqueRegistry := tools.NewWithStateDialect(root, &tools.SessionState{}, dialect)
	uniqueLines := read(uniqueRegistry, "unique.txt", map[string]any{}).([]hashline.Line)
	if _, err := edit(uniqueRegistry, "unique.txt", uniqueLines[1].Hash, []string{"BETA"}); err == nil {
		m.FirstAttemptSuccesses++
	}

	// 2: contextual hashes distinguish repeated closing braces by their
	// neighbors. Line hashes and exact text correctly reject the ambiguity.
	repeatedPath := write("repeated.go", []string{"func alpha()", "{", "}", "func beta()", "{", "}"})
	repeatedLines, err := hashline.ReadWithMode(repeatedPath, mode)
	if err != nil {
		panic(err)
	}
	_, err = hashline.ApplyWithMode(hashline.Patch{Path: repeatedPath, Hunks: []hashline.Hunk{{
		Anchor: repeatedLines[2].Hash,
		Delete: 1,
		Insert: []string{"} // alpha"},
	}}}, mode)
	if err == nil {
		m.FirstAttemptSuccesses++
	} else if isStale(err) {
		m.AmbiguitiesDetected++
	}

	// 3: changing a neighbor invalidates only a contextual anchor. This calls
	// the applier directly so optimistic locking does not hide anchor behavior.
	stalePath := write("stale.txt", []string{"left", "target", "right"})
	staleLines, err := hashline.ReadWithMode(stalePath, mode)
	if err != nil {
		panic(err)
	}
	write("stale.txt", []string{"changed", "target", "right"})
	_, err = hashline.ApplyWithMode(hashline.Patch{Path: stalePath, Hunks: []hashline.Hunk{{
		Anchor: staleLines[1].Hash,
		Delete: 1,
		Insert: []string{"TARGET"},
	}}}, mode)
	if isStale(err) {
		m.ContextStalenessDetected++
	}

	// 4: retrying the same stale anchor escalates from a fresh window to an
	// explicit region/directive response.
	write("ladder.txt", []string{"one", "two", "three"})
	ladderState := &tools.SessionState{}
	ladderRegistry := tools.NewWithStateDialect(root, ladderState, dialect)
	read(ladderRegistry, "ladder.txt", map[string]any{})
	missingAnchor := "deadbeef"
	if dialect == textDialect {
		missingAnchor = "line that is not present"
	}
	_, firstErr := edit(ladderRegistry, "ladder.txt", missingAnchor, []string{"TWO"})
	if isStale(firstErr) {
		m.Retries++
		ladderRegistry = tools.NewWithStateDialect(root, ladderState, dialect)
		secondResult, secondErr := edit(ladderRegistry, "ladder.txt", missingAnchor, []string{"TWO"})
		if isStale(secondErr) {
			if details, ok := secondResult.(map[string]any); ok && details["directive"] != nil && details["region"] != nil {
				m.EscalationsDetected++
			}
		}
	}

	// 5: three identical no-op edits produce one hard loop-breaker signal.
	write("noop.txt", []string{"same"})
	noopState := &tools.SessionState{}
	noopRegistry := tools.NewWithStateDialect(root, noopState, dialect)
	noopLines := read(noopRegistry, "noop.txt", map[string]any{}).([]hashline.Line)
	for range 3 {
		noopRegistry = tools.NewWithStateDialect(root, noopState, dialect)
		_, err := edit(noopRegistry, "noop.txt", noopLines[0].Hash, []string{"same"})
		if err != nil && strings.Contains(err.Error(), "E_NOOP_LOOP") {
			m.NoopLoopsDetected++
		}
	}

	// 6: a >2,000-line read returns hashed structural lines and supports a
	// cheap around_line recovery read.
	large := make([]string, 2105)
	for i := range large {
		large[i] = fmt.Sprintf("detail %d", i+1)
	}
	large[0] = "package large"
	large[1000] = "func landmark() {}"
	write("large.go", large)
	largeRegistry := tools.NewWithStateDialect(root, &tools.SessionState{}, dialect)
	outline := read(largeRegistry, "large.go", map[string]any{}).(map[string]any)
	outlineLines := outline["outline"].([]hashline.Line)
	window := read(largeRegistry, "large.go", map[string]any{"around_line": 1001, "limit": 5}).([]hashline.Line)
	m.LargeFileRecovery = outline["summarized"] == true && len(outlineLines) == 2 && outlineLines[1].Hash != "" && len(window) == 5

	// 7: a second session winning the write makes the first session reject its
	// edit with an optimistic-lock conflict.
	write("conflict.txt", []string{"session", "value"})
	a := tools.NewWithStateDialect(root, &tools.SessionState{}, dialect)
	b := tools.NewWithStateDialect(root, &tools.SessionState{}, dialect)
	aLines := read(a, "conflict.txt", map[string]any{}).([]hashline.Line)
	bLines := read(b, "conflict.txt", map[string]any{}).([]hashline.Line)
	if _, err := edit(b, "conflict.txt", bLines[1].Hash, []string{"B"}); err == nil {
		_, err = edit(a, "conflict.txt", aLines[1].Hash, []string{"A"})
		if err != nil && strings.Contains(err.Error(), "E_FILE_CHANGED") {
			m.ConflictsDetected++
		}
	}

	m.FirstAttemptRate = float64(m.FirstAttemptSuccesses) / float64(m.FirstAttemptOpportunities)
	return m
}

func benchmarkAnchorMode(dialect string) hashline.AnchorMode {
	switch dialect {
	case contextualDialect:
		return hashline.AnchorContextual
	case textDialect:
		return hashline.AnchorText
	default:
		return hashline.AnchorLine
	}
}

func isStale(err error) bool {
	if err == nil {
		return false
	}
	var stale *hashline.StaleError
	return errors.As(err, &stale) || strings.Contains(err.Error(), "stale or ambiguous anchor")
}

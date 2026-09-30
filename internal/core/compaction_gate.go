package core

import "fmt"

// compactionGate decides whether a phase change is worth a compaction. A
// compaction at a real phase boundary keeps durable state tidy, but each one
// costs a summary call and a cold cache. A session flipping between phases (as
// a completion loop does, review to wrap-up and back) would otherwise compact
// on every flip. The hard context ceiling is not gated.
type compactionGate struct {
	lastCompaction int
	compacted      bool
	// left records the turn each phase was last left.
	left map[string]int
}

const (
	// minTurnsBetweenPhaseCompactions spaces boundary compactions apart.
	minTurnsBetweenPhaseCompactions = 6
	// oscillationWindow: returning to a phase left this recently is a flip,
	// not a boundary.
	oscillationWindow = 8
	// minCompactionTokens: below this there is too little history to be
	// worth summarising.
	minCompactionTokens = 24_000
)

// phaseChange records a phase change at turn and reports whether to compact,
// with the reason when not.
func (g *compactionGate) phaseChange(from, to string, turn, inputTokens int) (bool, string) {
	if g.left == nil {
		g.left = map[string]int{}
	}
	leftAt, returning := g.left[to]
	g.left[from] = turn
	switch {
	case inputTokens < minCompactionTokens:
		return false, "history too small to summarise"
	case g.compacted && turn-g.lastCompaction < minTurnsBetweenPhaseCompactions:
		return false, "compacted recently"
	case returning && turn-leftAt <= oscillationWindow:
		return false, "returned to a phase left " + itoaTurns(turn-leftAt) + " ago"
	}
	return true, ""
}

func (g *compactionGate) record(turn int) {
	g.lastCompaction, g.compacted = turn, true
}

func itoaTurns(n int) string {
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

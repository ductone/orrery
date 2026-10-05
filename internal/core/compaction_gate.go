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
)

// phaseChange records a phase change at turn and reports whether to compact,
// with the reason when not. It compacts only past half the context window,
// and only at a clean boundary: leaving explore for plan or implement, or
// leaving wrap-up. Entering review or wrap-up, review back to implement or
// diagnose, and every other change keep the history intact.
func (g *compactionGate) phaseChange(from, to string, turn, inputTokens, window int) (bool, string) {
	if g.left == nil {
		g.left = map[string]int{}
	}
	leftAt, returning := g.left[to]
	g.left[from] = turn
	switch {
	case window <= 0 || inputTokens < window/2:
		return false, "history below half the context window"
	case !cleanPhaseBoundary(from, to):
		return false, "not a clean phase boundary"
	case g.compacted && turn-g.lastCompaction < minTurnsBetweenPhaseCompactions:
		return false, "compacted recently"
	case returning && turn-leftAt <= oscillationWindow:
		return false, "returned to a phase left " + itoaTurns(turn-leftAt) + " ago"
	}
	return true, ""
}

// cleanPhaseBoundary reports whether a phase change is a safe place to
// summarise: done exploring or wrapping up, never mid-verification.
func cleanPhaseBoundary(from, to string) bool {
	switch {
	case to == "review" || to == "wrap-up":
		return false
	case from == "review":
		return false
	case from == "explore":
		return to == "plan" || to == "implement"
	case from == "wrap-up":
		return true
	}
	return false
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

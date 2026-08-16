package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

type progressTracker struct {
	phase                         string
	phaseTurns, noProgressTurns   int
	repeatedReads, repeatedSearch int
	repeatedTodos                 int
	nudges, completionRejections  int
	delegated, edited, verified   bool
	reviewed                      bool
	seenResults                   map[string]string
	lastTodo                      string
	turnProgress                  bool
	turnEdited, turnVerified      bool
}

func newProgressTracker() *progressTracker {
	return &progressTracker{seenResults: map[string]string{}}
}

func (p *progressTracker) beginTurn(phase string) {
	if phase != p.phase {
		p.phase = phase
		p.phaseTurns = 0
		p.noProgressTurns = 0
		p.nudges = 0
	}
	p.phaseTurns++
	p.turnProgress = false
	p.turnEdited = false
	p.turnVerified = false
}

func (p *progressTracker) observe(call provider.ToolCall, value any, callErr error) any {
	name := call.Name
	if callErr == nil && (name == "read" || name == "search") {
		key := fingerprint(name, call.Arguments)
		resultHash := fingerprint("result", value)
		if p.seenResults[key] == resultHash {
			if name == "read" {
				p.repeatedReads++
			} else {
				p.repeatedSearch++
			}
			return map[string]any{
				"suppressed": true,
				"unchanged":  true,
				"hint":       "This exact result is unchanged from an earlier call. Use existing evidence, change the query/window, delegate exploration, or advance the todo.",
			}
		}
		p.seenResults[key] = resultHash
	}
	if callErr == nil {
		switch name {
		case "todo":
			todoHash := fingerprint("todo", call.Arguments)
			if p.lastTodo == todoHash {
				p.repeatedTodos++
				return map[string]any{
					"suppressed": true,
					"unchanged":  true,
					"hint":       "This todo is unchanged. Do not submit it again; gather new evidence or advance an item/phase.",
				}
			}
			p.lastTodo = todoHash
			p.turnProgress = true
		case "spawn":
			p.turnProgress = true
			p.delegated = true
		case "edit":
			p.turnProgress = true
			p.turnEdited = true
			p.edited = true
			p.verified = false
			p.reviewed = false
		case "exec":
			cmd := strings.ToLower(stringArg(call.Arguments, "command"))
			if containsAny(cmd, " test", "test ", "lint", "typecheck", "build", "check", "vet") {
				p.turnProgress = true
				p.turnVerified = true
				p.verified = true
			}
		}
	}
	return value
}

func (p *progressTracker) endTurn() {
	if p.turnProgress {
		p.noProgressTurns = 0
		return
	}
	p.noProgressTurns++
}

func (p *progressTracker) shouldDelegate() bool {
	return !p.delegated && p.phase == "explore" && p.noProgressTurns >= 3
}

func (p *progressTracker) shouldNudge() bool {
	return p.nudges == 0 && (p.phase == "explore" || p.phase == "plan") && (p.noProgressTurns >= 4 || p.phaseTurns >= 7)
}

func (p *progressTracker) markNudged() { p.nudges++ }

// terminalStallReason is deliberately narrow: an agent may spend many turns on
// a hard task, but repeatedly submitting the exact same plan after a progress
// intervention cannot create new evidence. Ending the run preserves budget and
// gives the caller an actionable failure instead of an unbounded loop.
func (p *progressTracker) terminalStallReason() string {
	if p.repeatedTodos >= 6 && p.noProgressTurns >= 6 {
		return "agent stalled after repeatedly submitting an unchanged todo plan"
	}
	return ""
}

func terminalPhaseStallReason(parentJob, phase string, phaseTurns int) string {
	if parentJob == "" && phaseTurns >= 12 && (phase == "review" || phase == "diagnose") {
		return "agent exceeded the bounded " + phase + " phase without reaching a terminal result"
	}
	return ""
}

func (p *progressTracker) stall() map[string]int {
	return map[string]int{
		"no_progress_turns": p.noProgressTurns,
		"phase_turns":       p.phaseTurns,
		"repeated_reads":    p.repeatedReads,
		"repeated_searches": p.repeatedSearch,
		"repeated_todos":    p.repeatedTodos,
	}
}

func (p *progressTracker) export(outcome *agentproto.Outcome) {
	outcome.NoProgressTurns = p.noProgressTurns
	outcome.DuplicateReads = p.repeatedReads
	outcome.DuplicateSearches = p.repeatedSearch
	outcome.ProgressNudges = p.nudges
	outcome.CompletionRejects = p.completionRejections
	outcome.ExplorationWorker = p.delegated
	outcome.Verified = p.verified
	outcome.IndependentlyReviewed = p.reviewed
}

func fingerprint(prefix string, value any) string {
	raw := store.JSON(value)
	sum := sha256.Sum256([]byte(prefix + "\x00" + raw))
	return hex.EncodeToString(sum[:8])
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func containsAny(s string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

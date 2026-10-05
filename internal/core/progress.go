package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
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
	completionRejections          int
	delegated, edited, verified   bool
	turnsSinceEdit                int
	reviewed                      bool
	reviewRemediation             bool
	seenResults                   map[string]string
	// floors memoises judge verdicts: when the judge declines to intervene, the
	// tripping signal's threshold is raised so the same question is not asked
	// again every turn. Scoped to the phase, like the counters themselves.
	floors                   map[string]int
	lastTodo                 string
	turnProgress             bool
	turnEdited, turnVerified bool
	// editedPaths are files changed through the edit tool this run.
	editedPaths map[string]bool
	// checksSinceEdit are successful commands run since the last edit that
	// were not recognised as verification; a classifier may still judge one a
	// meaningful check of the change.
	checksSinceEdit []commandRecord
	// verificationRejections bounds how often completion is refused for
	// missing verification, so an unverifiable change cannot loop.
	verificationRejections int
	verificationWaived     bool
	// fixPending is set by a review rejection and cleared by the next edit.
	fixPending bool
	// reviewRejections counts reviews that rejected this run's change.
	reviewRejections int
	// excluded are models taken out of this run for misbehaving.
	excluded []string
	// strikes count a model's repeated misbehaviour of one kind;
	// exclusionReasons say why each excluded model was set aside.
	strikes          map[string]int
	exclusionReasons map[string]string
	// rejectedDiff and rejectedReview hold the diff the last failed review
	// covered and its findings, so an unchanged diff is not reviewed again.
	rejectedDiff, rejectedReview string
	// answerRejections counts completions refused for answering something
	// other than the latest request.
	answerRejections int
	// formatVerified is set by a successful formatting or style check, which
	// verifies only non-code changes.
	formatVerified bool
}

// commandRecord is a command and the tail of its output.
type commandRecord struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

const maxChecksSinceEdit = 8

func newProgressTracker() *progressTracker {
	return &progressTracker{seenResults: map[string]string{}, floors: map[string]int{}}
}

func (p *progressTracker) beginTurn(phase string) {
	if phase != p.phase {
		p.phase = phase
		p.phaseTurns = 0
		p.noProgressTurns = 0
		clear(p.floors)
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
			p.checksSinceEdit = nil
			p.formatVerified = false
			p.fixPending = false
			if path := stringArg(call.Arguments, "path"); path != "" {
				if p.editedPaths == nil {
					p.editedPaths = map[string]bool{}
				}
				p.editedPaths[path] = true
			}
		case "exec":
			command := stringArg(call.Arguments, "command")
			switch verificationKind(command) {
			case fullCheck:
				p.turnProgress = true
				p.turnVerified = true
				p.verified = true
			case formatCheck:
				// Counts for prose and configuration, not for code; see
				// verificationSatisfied.
				p.turnProgress = true
				p.formatVerified = true
			default:
				if !p.edited {
					break
				}
				p.checksSinceEdit = append(p.checksSinceEdit, commandRecord{Command: command, Output: outputTail(value)})
				if len(p.checksSinceEdit) > maxChecksSinceEdit {
					p.checksSinceEdit = p.checksSinceEdit[1:]
				}
			}
		}
	}
	return value
}

// outputTail keeps the end of a command's summarised output for a classifier.
func outputTail(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["summary"].(string)
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return s
}

func (p *progressTracker) endTurn() {
	if p.turnEdited {
		p.turnsSinceEdit = 0
	} else {
		p.turnsSinceEdit++
	}
	if p.turnProgress {
		p.noProgressTurns = 0
		return
	}
	p.noProgressTurns++
}

func (p *progressTracker) shouldDelegate() bool {
	return !p.delegated && p.phase == "explore" && p.noProgressTurns >= 3
}

func (p *progressTracker) shouldForcePlanExecution() bool {
	return p.repeatedTodos >= 2 || p.phaseTurns >= 6
}

func (p *progressTracker) shouldForceVerifiedCompletion() bool {
	return p.verified && p.turnsSinceEdit >= 3 && (p.phase == "review" || p.phase == "diagnose")
}

func shouldForceFinalResolution(phase string, phaseTurns int) bool {
	return (phase == "review" || phase == "diagnose") && phaseTurns >= 9
}

// strike records one misbehaviour of a kind by a model and reports whether
// it is the third, resetting the count when it is.
func (p *progressTracker) strike(model, kind string) bool {
	if p.strikes == nil {
		p.strikes = map[string]int{}
	}
	key := model + "|" + kind
	p.strikes[key]++
	if p.strikes[key] >= 3 {
		delete(p.strikes, key)
		return true
	}
	return false
}

// exclude takes a misbehaving model out of routing for the rest of the run.
func (p *progressTracker) exclude(model string) {
	if !slices.Contains(p.excluded, model) {
		p.excluded = append(p.excluded, model)
	}
}

// markReviewRejected records a rejection; reviewed is false when the diff
// was refused unchanged without running a reviewer.
func (p *progressTracker) markReviewRejected(reviewed bool) {
	p.reviewRemediation = true
	if reviewed {
		p.reviewRejections++
	}
	p.fixPending = true
}

// awaitingFix reports a review rejection that no edit has answered yet. The
// "finish now" modes that turn tool calls off must not apply then: they
// once left an agent unable to edit after a failed review, so every turn
// produced the same diff and another identical review.
func (p *progressTracker) awaitingFix() bool { return p.reviewRemediation && p.fixPending }

// maxReviewRejections bounds how many independent reviews may reject a run's
// change before it stops.
const maxReviewRejections = 4

func (p *progressTracker) reviewRemediationReason(parentJob string) string {
	if parentJob != "" || !p.reviewRemediation {
		return ""
	}
	if p.reviewRejections >= maxReviewRejections {
		return fmt.Sprintf("independent review rejected the change %d times", p.reviewRejections)
	}
	return ""
}

// threshold returns the effective trigger level for a signal: the built-in base
// unless the judge has raised a floor for it in this phase.
func (p *progressTracker) threshold(signal string, base int) int {
	if floor, ok := p.floors[signal]; ok && floor > base {
		return floor
	}
	return base
}

// backoff records a "not stuck" verdict by raising the signal's floor above the
// value that just tripped, multiplicatively. The multiplier is scale-free: the
// right level differs per task and is unknown up front, so this converges on it
// in a logarithmic number of judge calls rather than a linear one.
func (p *progressTracker) backoff(signal string, observed int, factor float64) {
	next := int(math.Ceil(float64(observed) * factor))
	if next <= observed {
		next = observed + 1
	}
	if next > p.floors[signal] {
		p.floors[signal] = next
	}
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

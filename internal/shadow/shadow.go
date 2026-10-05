// Package shadow defines the questions Orrery asks a shadow classifier at its
// decision sites, and how recorded answers are compared with what the harness
// actually decided. Answers never feed back into routing.
//
// Each request site has a question version. Changing a question's wording or
// criteria must bump its version, because observations under different
// questions are not comparable.
package shadow

import "github.com/ductone/orrey/internal/jev"

// Request sites, as stored with each observation.
const (
	// Turn cross-checks the declared phase and scores difficulty at turn start.
	Turn = "turn"
	// Spawn scores a worker spec's difficulty at job creation.
	Spawn = "spawn"
	// ReviewRisk predicts the independent reviewer's verdict from the diff.
	ReviewRisk = "review_risk"
	// ReviewVerdict reads an inconclusive reviewer's final output.
	ReviewVerdict = "review_verdict"
	// MemorySelect ranks candidate memory records for the pinned set at a
	// cache-safe boundary. Gated independently by memory.jev.selection;
	// shadow-only until promoted.
	MemorySelect = "memory_select"
	// CompactionBenefit scores whether compacting now is worth its cost.
	// Gated independently by memory.jev.compaction_benefit; shadow-only
	// until promoted.
	CompactionBenefit = "compaction_benefit"
)

const (
	TurnVersion              = "turn/v1"
	SpawnVersion             = "spawn/v1"
	ReviewRiskVersion        = "review_risk/v1"
	ReviewVerdictVersion     = "review_verdict/v1"
	MemorySelectVersion      = "memory_select/v1"
	CompactionBenefitVersion = "compaction_benefit/v1"
)

// Phases mirrors router phases; the classifier chooses among them.
var phaseCriteria = map[string]string{
	"explore":   "Gathering information: reading files, searching the codebase, or inspecting the environment to understand the task.",
	"plan":      "Deciding an approach or breaking the work into steps before changing code.",
	"implement": "Making the planned code changes.",
	"diagnose":  "Investigating why something fails: a failing test, a build error, or unexpected behaviour.",
	"review":    "Checking completed changes for correctness: reading the diff, running verification, or addressing review findings.",
	"wrap-up":   "The work is done; the agent is summarising results or reporting back.",
}

// TurnQuestions cross-checks the phase. (A difficulty score was asked here
// and at spawn until shadow data showed it carried no signal.)
func TurnQuestions(phase bool) map[string]jev.Question {
	q := map[string]jev.Question{}
	if phase {
		q["phase"] = jev.Choice("Which phase of work is this coding agent in right now, judged from its plan and recent activity?", phaseCriteria)
	}
	return q
}

func ReviewRiskQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		"introduces_bug": jev.Noul(
			"Does this diff introduce a correctness bug?",
			"The patch contains a logic error, a broken edge case, wrong API use, a race, or a regression that a careful reviewer would flag.",
			"The patch appears correct for its task; any remaining issues are style or preference.",
		),
		"risk": jev.Score("How risky is this change to merge without an independent review?",
			"Low: a small, mechanical, or well-contained change.",
			"Medium: changes behaviour in a contained area.",
			"High: a broad or subtle behavioural change, or one touching concurrency, security, or data handling.",
		),
	}
}

func ReviewVerdictQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		"verdict": jev.Choice("Based on the reviewer's final output, what did it conclude about the patch?", map[string]string{
			"pass":       "The reviewer concluded the patch has no correctness bugs.",
			"fail":       "The reviewer identified at least one correctness bug introduced by the patch.",
			"no_verdict": "The reviewer stopped before reaching a conclusion about the patch.",
		}),
	}
}

// MemoryCandidate is one bounded candidate description sent to Jev for
// memory_select: metadata and a short excerpt only, never the full record
// text, workspace identity, or other session content.
type MemoryCandidate struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Scope      string  `json:"scope"`
	Confidence float64 `json:"confidence"`
	AgeDays    int     `json:"age_days"`
}

// MemorySelectQuestions asks, per candidate, whether it is relevant to the
// current task. One Choice question per candidate keeps each judgement atomic
// and decomposable rather than one opaque "what should I remember" prompt.
func MemorySelectQuestions(candidates []MemoryCandidate) map[string]jev.Question {
	q := map[string]jev.Question{}
	for _, c := range candidates {
		q[c.ID] = jev.Choice("Is this candidate memory relevant to the current task and query?", map[string]string{
			"include": "Directly useful for the current task: a fact, decision, or lesson that bears on it.",
			"maybe":   "Possibly relevant but tangential or stale.",
			"exclude": "Not relevant to the current task.",
		})
	}
	return q
}

// CompactionBenefitQuestions asks whether compacting now is worth the token
// cost and cache invalidation, given estimated remaining tokens and the value
// of retaining the recent tail.
func CompactionBenefitQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		"compact_now": jev.Choice("Given the estimated remaining context budget and the value of the recent exact tail, should this agent compact its history now?", map[string]string{
			"compact": "Context pressure or stale bulk outweighs the value of keeping the recent tail uncompacted.",
			"wait":    "There is enough headroom and the recent tail is still valuable verbatim.",
		}),
	}
}

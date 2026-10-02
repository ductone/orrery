// Package shadow defines the questions Orrery asks a shadow classifier at its
// decision sites, and how recorded answers are compared with what the harness
// actually decided. Answers never feed back into routing or interventions.
//
// Each request site has a question version. Changing a question's wording or
// criteria must bump its version, because observations under different
// questions are not comparable.
package shadow

import "github.com/ductone/orrey/internal/jev"

// Request sites, as stored with each observation.
const (
	// StallJudge mirrors the LLM stall judge on the same evidence.
	StallJudge = "stall_judge"
	// Turn cross-checks the declared phase and scores difficulty at turn start.
	Turn = "turn"
	// Spawn scores a worker spec's difficulty at job creation.
	Spawn = "spawn"
	// ReviewRisk predicts the independent reviewer's verdict from the diff.
	ReviewRisk = "review_risk"
	// ReviewVerdict reads an inconclusive reviewer's final output.
	ReviewVerdict = "review_verdict"
)

const (
	StallJudgeVersion    = "stall_judge/v1"
	TurnVersion          = "turn/v1"
	SpawnVersion         = "spawn/v1"
	ReviewRiskVersion    = "review_risk/v1"
	ReviewVerdictVersion = "review_verdict/v1"
)

func StallQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		"stuck": jev.Noul(
			"Is this autonomous coding agent genuinely stuck, rather than making steady progress?",
			"It repeats equivalent calls, re-reads content it already has, oscillates between the same few actions, or shows no path from its recent activity to the task.",
			"Its recent calls gather new information that plausibly advances the task: reading different files, searching different terms, narrowing toward a location, or verifying a change.",
		),
		"stall_kind": jev.Choice("What best explains the agent's recent lack of measurable progress?", map[string]string{
			"not_stuck":           "The agent is making real progress; the counters are tripping on productive reading or searching.",
			"capability":          "The problem is genuinely hard: reasonable attempts keep failing, such as a fix that does not work or a test that fails for a subtle reason.",
			"discipline":          "Redundant exploration: re-reading or re-searching what the agent already has instead of acting on it.",
			"environment":         "The environment is broken: a missing dependency, a service that is down, permission denied, command not found, or a network failure.",
			"missing_information": "The agent needs information from the user that it cannot find in the workspace.",
		}),
	}
}

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

package agentproto

import "time"

type Status string

const (
	Pass            Status = "pass"
	Fail            Status = "fail"
	BudgetExhausted Status = "budget_exhausted"
	Cancelled       Status = "cancelled"
	InputRequired   Status = "input_required"
)

type InputRequest struct {
	ID            string   `json:"id"`
	Question      string   `json:"question"`
	Choices       []string `json:"choices,omitempty"`
	AllowFreeform bool     `json:"allow_freeform"`
	// Kind says why the session paused: InputAsk, InputBudget or InputLimit.
	Kind string `json:"kind,omitempty"`
	// For InputBudget: the session's budget and what it has spent (including
	// reserved worker budget), and what agreeing to continue adds to the budget.
	BudgetUSD    float64 `json:"budget_usd,omitempty"`
	SpentUSD     float64 `json:"spent_usd,omitempty"`
	ExtensionUSD float64 `json:"extension_usd,omitempty"`
}

// The kinds of pause an InputRequest can be.
const (
	// InputAsk is a question the model asked with its ask tool.
	InputAsk = "ask"
	// InputBudget asks whether to extend the session's dollar budget.
	InputBudget = "budget"
	// InputLimit asks whether to keep going after a limit or an error.
	InputLimit = "limit"
)

// The answers the harness's own questions (InputBudget and InputLimit) offer
// besides extending the budget.
const (
	ChoiceContinue = "Keep going"
	ChoiceStop     = "Stop here"
)

type ContinueRequest struct{ SessionID, Content, InputID string }

type Budget struct {
	MaxTokens    int           `json:"max_tokens"`
	MaxUSD       float64       `json:"max_usd"`
	MaxWallClock time.Duration `json:"max_wallclock"`
	MaxDepth     uint32        `json:"max_depth"`
}
type Workspace struct {
	Path      string `json:"path"`
	Mode      string `json:"mode"`
	Ownership string `json:"ownership,omitempty"`
}
type RoutingHints struct {
	TierPin           string   `json:"tier_pin,omitempty"`
	FamilyExcludes    []string `json:"family_excludes,omitempty"`
	Review            bool     `json:"review,omitempty"`
	ImplementerFamily string   `json:"implementer_family,omitempty"`
	// WorkerTurns is the turn at which a read-only worker must start
	// synthesising its result. Zero selects the harness default.
	WorkerTurns int `json:"worker_turns,omitempty"`
	// Model and Effort pin this request's own model calls to one route (id or
	// canonical model name) and one reasoning effort, for benchmark sweeps.
	// Child jobs do not inherit them.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Background marks work nobody is waiting on; routing then values time
	// at the background rate. Child jobs inherit it.
	Background bool `json:"background,omitempty"`
}
type AttachmentRef struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
}
type TaskRequest struct {
	Spec         string          `json:"spec"`
	ResultSchema map[string]any  `json:"result_schema,omitempty"`
	Budget       Budget          `json:"budget"`
	Workspace    Workspace       `json:"workspace"`
	Hints        RoutingHints    `json:"hints,omitempty"`
	Depth        uint32          `json:"depth"`
	Attachments  []AttachmentRef `json:"attachments,omitempty"`
}
type Outcome struct {
	Tokens                int           `json:"tokens"`
	CostUSD               float64       `json:"cost_usd"`
	Latency               time.Duration `json:"latency"`
	ToolCalls             int           `json:"tool_calls"`
	ToolErrors            int           `json:"tool_errors"`
	EditAttempts          int           `json:"edit_attempts"`
	EditRetries           int           `json:"edit_retries"`
	NoProgressTurns       int           `json:"no_progress_turns,omitempty"`
	DuplicateReads        int           `json:"duplicate_reads,omitempty"`
	DuplicateSearches     int           `json:"duplicate_searches,omitempty"`
	ProgressNudges        int           `json:"progress_nudges,omitempty"`
	CompletionRejects     int           `json:"completion_rejections,omitempty"`
	ExplorationWorker     bool          `json:"exploration_worker,omitempty"`
	Verified              bool          `json:"verified,omitempty"`
	ReviewDisputed        bool          `json:"review_disputed,omitempty"`
	IndependentlyReviewed bool          `json:"independently_reviewed,omitempty"`
	BudgetReason          string        `json:"budget_reason,omitempty"`
}
type ArtifactRef struct{ Path, Description string }
type TaskResult struct {
	Status    Status         `json:"status"`
	Result    map[string]any `json:"result,omitempty"`
	Outcome   Outcome        `json:"outcome"`
	Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
	Error     string         `json:"error,omitempty"`
}
type AgentEvent struct {
	Type     string      `json:"type"`
	Data     any         `json:"data"`
	Terminal *TaskResult `json:"terminal,omitempty"`
}

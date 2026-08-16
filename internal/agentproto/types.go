package agentproto

import "time"

type Status string

const (
	Pass            Status = "pass"
	Fail            Status = "fail"
	BudgetExhausted Status = "budget_exhausted"
	Cancelled       Status = "cancelled"
)

type Budget struct {
	MaxTokens    int           `json:"max_tokens"`
	MaxUSD       float64       `json:"max_usd"`
	MaxWallClock time.Duration `json:"max_wallclock"`
	MaxDepth     uint32        `json:"max_depth"`
}
type Workspace struct {
	Path      string `json:"path"`
	Isolation string `json:"isolation"`
	Ownership string `json:"ownership,omitempty"`
}
type RoutingHints struct {
	TierPin           string   `json:"tier_pin,omitempty"`
	FamilyExcludes    []string `json:"family_excludes,omitempty"`
	Review            bool     `json:"review,omitempty"`
	ImplementerFamily string   `json:"implementer_family,omitempty"`
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

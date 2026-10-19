package storage

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrMemoryNotFound        = errors.New("memory record not found")
	ErrMemoryNotSupersedable = errors.New("memory record cannot be superseded")
)

var MemoryKinds = []string{"fact", "command", "decision", "preference", "lesson"}
var MemoryProvenances = []string{"user", "instruction", "observed", "extracted"}

type Session struct {
	ID, Spec, Title, DurableSummary, Phase, Model, Status string
	Integration, ExternalID, ExternalIncarnation          string
	WorkspacePath, WorkspaceOwnership                     string
	ParentSessionID                                       string
	IntegrationContextJSON, RequestJSON                   string
	Turn                                                  int
	SpentUSD, BudgetUSD                                   float64
	CreatedAt, UpdatedAt                                  time.Time
}

type Message struct {
	Role, ContentJSON string
	CreatedAt         time.Time
}

type Checkpoint struct {
	ID, SessionID, Label, Reason                       string
	SessionJSON, MessagesJSON, TodosJSON               string
	PendingInputsJSON, WorkItemsJSON, ContinuationJSON string
	CreatedAt                                          time.Time
}

type PendingInput struct {
	ID, SessionID, Question, Answer, Status string
	Choices                                 []string
	AllowFreeform                           bool
	CreatedAt, AnsweredAt                   time.Time
}

type QueuedMessage struct {
	ID              int
	SessionID       string
	RequestID       string
	Content         string
	AttachmentsJSON string
	Source          string
	PayloadHash     string
	CreatedAt       time.Time
}

type Todo struct {
	Position int    `json:"position"`
	Text     string `json:"text"`
	Phase    string `json:"phase"`
	Status   string `json:"status"`
}

// WorkItem is a harness-owned continuation record derived from the todo plan.
// Unlike todo prose, it carries a stable ID and lifecycle timestamps so the
// active objective and report obligation survive compaction and todo edits.
type WorkItem struct {
	ID, SessionID, Objective, Status, Phase string
	Position                                int
	CreatedAt, CompletedAt                  time.Time
}

// Continuation is the authoritative per-session continuation state: which work
// item is active and whether a final report is still owed.
type Continuation struct {
	SessionID           string
	ActiveWorkItemID    string
	FinalReportRequired bool
	ResolvedRequestIDs  []string
	UpdatedAt           time.Time
}

type Job struct {
	ID, SessionID, ParentJobID, Spec, ResultSchemaJSON, BudgetJSON, WorkspaceJSON, HintsJSON, Model, Status, ResultJSON, OutcomeJSON string
	Depth                                                                                                                            int
	CreatedAt, UpdatedAt                                                                                                             time.Time
}

type Event struct {
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	Seq           int             `json:"sequence"`
	SessionID     string          `json:"session_id"`
	TurnID        string          `json:"turn_id,omitempty"`
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data"`
	CreatedAt     time.Time       `json:"timestamp"`
}

type RequestReceipt struct {
	SessionID, RequestID, Kind, TurnID, PayloadHash string
	AcceptedAt                                      time.Time
	Duplicate                                       bool
}

type CacheEntry struct {
	SessionID, Model string
	WarmPrefixTokens int
	LastHit          time.Time
	TTL              time.Duration
}

type RoutingRecord struct {
	ID, SessionID                                                       string
	Turn                                                                int
	DecisionPoint, StateJSON, CandidatesJSON, ChosenModel, ChosenEffort string
	WasSwitch                                                           bool
	CacheEstJSON, Explanation                                           string
}

type Workspace struct {
	ID, IdentityKey      string
	CreatedAt, UpdatedAt time.Time
}

type MemoryRecord struct {
	ID, WorkspaceID, Scope, Kind, Text, Provenance string
	Confidence                                     float64
	Status                                         string
	EvidenceRefs                                   string
	CreatedAt, UpdatedAt                           time.Time
	ExpiresAt                                      *time.Time
	SupersededBy                                   string
}

type MemoryFilter struct {
	WorkspaceID    string
	Scope          string
	Status         string
	IncludeExpired bool
	// IncludeDeleted also returns forget tombstones, so extraction can avoid
	// resurrecting something the person explicitly forgot.
	IncludeDeleted bool
}

// MemoryMutation describes one lifecycle change committed with its audit event.
type MemoryMutation struct {
	Operation   string
	Record      MemoryRecord
	WorkspaceID string
	OldID       string
	MemoryID    string
}

// ModelOutcome is what happened to the work a route did. Authored counts are
// fractional: a review verdict is shared among the routes that made the edit
// and exec-write calls since the previous review, by share of calls.
type ModelOutcome struct {
	Route                      string
	AuthoredPass, AuthoredFail float64
	// Reviews counts reviews the route performed; Overturned those whose
	// findings were later overturned by adjudication or a later review.
	Reviews, Overturned                              int
	AnswerRejected                                   int
	RunPass, RunFail, RunInputRequired, RunCancelled int
}

type ModelStat struct {
	Route                                       string
	Calls                                       int
	LatencySeconds, OutputTokensPerSecond       float64
	OutputTokensPerCall                         float64
	InputTokens, CacheReadTokens                int
	Truncated, Empty, Malformed, ProviderErrors int
	LastCall, LastSlowCall, UpdatedAt           time.Time
}

func (e CacheEntry) Valid(now time.Time) bool {
	return e.WarmPrefixTokens > 0 && !e.LastHit.IsZero() && now.Before(e.LastHit.Add(e.TTL))
}

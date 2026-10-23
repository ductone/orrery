package storage

import (
	"encoding/json"
	"errors"
	"strings"
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
	// ArchivedAt is when the session was archived (see Archiver); the zero
	// time means it has not been.
	ArchivedAt time.Time
}

// SessionArchive is what Archiver.ArchiveSession removed from a session.
type SessionArchive struct {
	Events        []Event
	Messages      []Message
	Todos         []Todo
	WorkItems     []WorkItem
	Continuation  Continuation
	PendingInputs []PendingInput
	Checkpoints   []Checkpoint
	ArchivedAt    time.Time
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

// Todo statuses. A dropped item is one the agent decided is no longer needed;
// like a completed one it is closed and does not hold a session open.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
	TodoDropped    = "dropped"
)

// TodoOpen reports whether a todo status still needs work: pending or
// in_progress.
func TodoOpen(status string) bool { return status == TodoPending || status == TodoInProgress }

// OpenTodos returns the text of each todo that is still pending or in
// progress, in plan order.
func OpenTodos(ts []Todo) []string {
	var open []string
	for _, t := range ts {
		if TodoOpen(t.Status) && strings.TrimSpace(t.Text) != "" {
			open = append(open, strings.TrimSpace(t.Text))
		}
	}
	return open
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

// Memory scopes. Only ScopeAgent is written today; ScopeGroup and ScopeGlobal
// are reserved for cross-agent promotion. The names "workspace", "project" and
// "user" are the earlier spellings of agent, group and global and are accepted
// on input (see NormalizeMemoryScope).
const (
	ScopeAgent  = "agent"
	ScopeGroup  = "group"
	ScopeGlobal = "global"
)

type MemoryRecord struct {
	ID, WorkspaceID, Scope, Kind, Text, Provenance string
	Confidence                                     float64
	Status                                         string
	EvidenceRefs                                   string
	CreatedAt, UpdatedAt                           time.Time
	ExpiresAt                                      *time.Time
	SupersededBy                                   string
	// SessionID is the session that created the record and Origin the
	// workspace identity key (agent identity) of its writer. Both are empty
	// for records written before provenance was tracked.
	SessionID, Origin string
	// Shadows is the ID of a broader-scope (group or global) record this
	// agent-scope record overrides for its own agent. Only agent-scope
	// records may set it.
	Shadows string
}

type MemoryFilter struct {
	WorkspaceID string
	// Scope restricts to one scope; kept for compatibility, equivalent to
	// Scopes with a single entry. Old scope names are accepted.
	Scope string
	// Scopes restricts to any of the named scopes; empty (with Scope empty)
	// means all scopes.
	Scopes         []string
	Status         string
	IncludeExpired bool
	// IncludeDeleted also returns forget tombstones, so extraction can avoid
	// resurrecting something the person explicitly forgot.
	IncludeDeleted bool
}

// MemoryMutation describes one lifecycle change committed with its audit
// event. Operation is one of
//
//   - "commit": insert Record.
//   - "update": rewrite Record (by ID), which is neither deleted nor
//     superseded.
//   - "correct": insert Record and supersede OldID with it.
//   - "forget": make MemoryID (in WorkspaceID) a deleted tombstone.
//   - "restore": make MemoryID (in WorkspaceID), which is not active, active
//     again, with no SupersededBy and Record.ExpiresAt as its expiry.
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

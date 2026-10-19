// Package storage defines the record types and interfaces through which the
// Orrery engine persists its state, so a host can supply its own backend.
//
// Store is the full set the engine needs. It is split into smaller
// interfaces by kind of state for documentation and for narrower fakes, but
// an implementation provides them together: several methods change more than
// one kind of state atomically (accepting a message writes a receipt, a
// message and an event; restoring a checkpoint rewrites the session,
// transcript, todos and pending inputs). The internal SQLite store is the
// reference implementation.
//
// A store whose writes become durable only when committed (an LSM flushed to
// object storage, say) also implements Committer; the engine calls Commit at
// step boundaries. Stores whose writes are already durable, like SQLite, need
// not implement it.
//
// Methods that look up one record return an error matching ErrNotFound (via
// errors.Is) when it does not exist. JSON-valued parameters typed any are
// marshaled with encoding/json and read back as the raw JSON string fields of
// the record types.
package storage

import (
	"context"
	"database/sql"
	"time"
)

// ErrNotFound is returned when a requested record does not exist. It is the
// same value as sql.ErrNoRows, so the SQLite implementation and code written
// against it agree; other implementations return (or wrap) ErrNotFound.
var ErrNotFound = sql.ErrNoRows

// Store is everything the engine persists.
type Store interface {
	Sessions
	Transcript
	Events
	Inbox
	Jobs
	Memory
	Routing
	RouteStats
}

// Committer is implemented by stores whose writes become durable only when
// committed. The engine calls Commit at step boundaries; when it returns
// nil, everything written before the call must be durable.
type Committer interface {
	Commit(ctx context.Context) error
}

// Sessions holds session records, their status and their dollar accounting.
type Sessions interface {
	// CreateSession inserts a new session. Empty Phase, Status,
	// WorkspaceOwnership, IntegrationContextJSON, RequestJSON and Title are
	// defaulted to "plan", "running", "orrery", "{}", "{}" and
	// SessionTitle(Spec).
	CreateSession(ctx context.Context, x Session) error
	// CreateSessionAccepted atomically creates an integration-owned session
	// (Integration and ExternalID required), records requestID as accepted
	// (receipt kind "integration"), and appends the events "session.created"
	// and "turn.accepted" as sequences 1 and 2. If a session already exists
	// for (Integration, ExternalID, ExternalIncarnation) it returns that
	// session and false without writing anything.
	CreateSessionAccepted(ctx context.Context, x Session, requestID, turnID, payloadHash string) (Session, bool, error)
	Session(ctx context.Context, id string) (Session, error)
	SessionByExternalID(ctx context.Context, integration, externalID, incarnation string) (Session, error)
	// Sessions lists top-level sessions (no parent), most recently updated
	// first.
	Sessions(ctx context.Context) ([]Session, error)
	// ChildSessions lists every child session (one with a ParentSessionID:
	// the workers and reviewers a session spawned), including any whose
	// parent has since been deleted, most recently updated first.
	ChildSessions(ctx context.Context) ([]Session, error)
	// UpdateSession writes DurableSummary, Phase, Model, Turn and Status.
	UpdateSession(ctx context.Context, x Session) error
	UpdateSessionTitle(ctx context.Context, id, title string) error
	SetSessionStatus(ctx context.Context, id, status string) error
	// MarkRunningInterrupted marks sessions left "running" by a process that
	// no longer exists as "interrupted", so they can be resumed. Sessions a
	// live process is running are left alone. A store with single-owner
	// fencing (opening it fences out any previous owner) may mark every
	// running session interrupted, since no other process can be running
	// them.
	MarkRunningInterrupted(ctx context.Context) error
	// DeleteSession removes a session and everything recorded for it.
	DeleteSession(ctx context.Context, id string) error
	// ForkSession creates newID as an independent branch of sourceID sharing
	// its workspace path. It copies the durable summary, messages and todos
	// but starts a new routing history, a new cost meter, and a new event
	// log whose first event is "session.forked".
	ForkSession(ctx context.Context, sourceID, newID string) (Session, error)
	// AddSpend adds usd to the session's spend.
	AddSpend(ctx context.Context, id string, usd float64) error
	// AddBudget raises the session's dollar ceiling by usd.
	AddBudget(ctx context.Context, id string, usd float64) error
	// ReservedJobUSD sums the dollar budgets of the session's running jobs.
	ReservedJobUSD(ctx context.Context, sid string) (float64, error)
	// SpentUSDWithChildren returns the spend of the session plus its direct
	// child sessions.
	SpentUSDWithChildren(ctx context.Context, sid string) (float64, error)
}

// Transcript holds the conversation a model is sent, the todo list and work
// ledger derived from it, and checkpoints of all of these.
type Transcript interface {
	AddMessage(ctx context.Context, sid, role string, content any) error
	// Messages returns the session's messages in insertion order.
	Messages(ctx context.Context, sid string) ([]Message, error)
	// ReplaceMessages replaces the session's messages with msgs.
	ReplaceMessages(ctx context.Context, sid string, msgs []Message) error
	// ShrinkMessages passes each message of role whose content exceeds
	// maxChars through shrink, stores the result, and returns how many
	// changed.
	ShrinkMessages(ctx context.Context, sid, role string, maxChars int, shrink func(string) string) (int, error)
	// ApplyCompaction atomically writes session.DurableSummary, keeps only
	// the newest keep messages, and clears the session's cache warmth.
	ApplyCompaction(ctx context.Context, session Session, keep int) error
	// AcknowledgeReport clears the continuation's report obligation and
	// active work item after a final report and replaces the durable summary.
	AcknowledgeReport(ctx context.Context, sid, durableSummary string) error
	// SetTodos replaces the todo list and, in the same transaction, applies
	// SyncWorkItems: it stores the returned work items (deleting the rest)
	// and sets the continuation's ActiveWorkItemID and FinalReportRequired,
	// keeping its ResolvedRequestIDs.
	SetTodos(ctx context.Context, sid string, todos []Todo) error
	Todos(ctx context.Context, sid string) ([]Todo, error)
	WorkItems(ctx context.Context, sid string) ([]WorkItem, error)
	// Continuation returns the session's continuation ledger, or the zero
	// value when none exists.
	Continuation(ctx context.Context, sid string) (Continuation, error)
	// CreateCheckpoint snapshots the session, messages, todos, pending inputs,
	// work items and continuation.
	CreateCheckpoint(ctx context.Context, id, sid, label, reason string) (Checkpoint, error)
	Checkpoints(ctx context.Context, sid string) ([]Checkpoint, error)
	Checkpoint(ctx context.Context, id string) (Checkpoint, error)
	// RestoreCheckpoint atomically rewinds everything CreateCheckpoint
	// captured and leaves the session "interrupted".
	RestoreCheckpoint(ctx context.Context, sid, checkpointID string) error
}

// Events is the session's append-only event log.
type Events interface {
	// AddEvent appends an event with the session's next sequence number.
	AddEvent(ctx context.Context, sid, typ string, data any) (Event, error)
	AddEventForTurn(ctx context.Context, sid, turnID, typ string, data any) (Event, error)
	// EventsAfter returns events with sequence greater than after, in order.
	EventsAfter(ctx context.Context, sid string, after int) ([]Event, error)
}

// Inbox holds what arrives for a session from outside: answers to its
// questions, queued messages, and idempotency receipts for accepted requests.
type Inbox interface {
	// CreatePendingInput records a question the session is waiting on.
	CreatePendingInput(ctx context.Context, input PendingInput) error
	// PendingInput returns the session's newest unanswered question.
	PendingInput(ctx context.Context, sid string) (PendingInput, error)
	// PendingInputs returns every question recorded for the session, oldest
	// first.
	PendingInputs(ctx context.Context, sid string) ([]PendingInput, error)
	// ResolvePendingInput answers the newest unanswered question. When the
	// question has choices and disallows free-form answers, answer must be one
	// of them.
	ResolvePendingInput(ctx context.Context, sid, answer string) (PendingInput, error)
	// EnqueueMessage stores a message to deliver as the next turn. It
	// returns true when the message was already queued: a retry with the
	// same requestID and payloadHash is a no-op. Reusing requestID for
	// different content is an error.
	EnqueueMessage(ctx context.Context, sessionID, requestID, content, source, payloadHash string, attachments any) (bool, error)
	// DequeueMessage removes and returns the oldest queued message.
	DequeueMessage(ctx context.Context, sessionID string) (QueuedMessage, bool, error)
	// DeliverQueuedMessage does what AcceptMessage does for a message
	// already removed with DequeueMessage.
	DeliverQueuedMessage(ctx context.Context, sid, requestID, turnID, kind, payloadHash string, content, request any) (RequestReceipt, error)
	// AcceptMessage atomically records the request receipt, appends content
	// as a user message, appends the events "turn.accepted" and
	// "user.message", and sets LatestRequest as RequestText directs. A retry
	// with the same
	// request ID and payload returns the original receipt with Duplicate set
	// and writes nothing; reusing the ID for different content is an error.
	AcceptMessage(ctx context.Context, sid, requestID, turnID, kind, payloadHash string, content, request any) (RequestReceipt, error)
	RequestReceipt(ctx context.Context, sid, requestID string) (RequestReceipt, error)
	// LatestRequest returns the most recent message a person sent to the
	// session (see AcceptMessage), surviving compaction; "" if none beyond
	// the initial task.
	LatestRequest(ctx context.Context, sid string) (string, error)
}

// Jobs holds worker jobs spawned by sessions.
type Jobs interface {
	CreateJob(ctx context.Context, j Job) error
	FinishJob(ctx context.Context, id, status string, result, outcome any) error
	Job(ctx context.Context, id string) (Job, error)
	Jobs(ctx context.Context, sid string) ([]Job, error)
}

// Memory holds durable memory records, partitioned by workspace.
type Memory interface {
	// EnsureWorkspace returns the workspace for an identity key, creating it
	// if needed.
	EnsureWorkspace(ctx context.Context, identityKey string) (Workspace, error)
	// MemoryWorkspaces lists every workspace that has memory records.
	MemoryWorkspaces(ctx context.Context) ([]Workspace, error)
	// CommitMemory inserts a record after ApplyMemoryDefaults and
	// ValidateMemoryRecord, generating an ID if it has none.
	CommitMemory(ctx context.Context, x MemoryRecord) (MemoryRecord, error)
	// UpdateMemory rewrites a record after ApplyMemoryDefaults and
	// ValidateMemoryRecord.
	UpdateMemory(ctx context.Context, x MemoryRecord) error
	// SupersedeMemory marks oldID superseded by newID; both must belong to
	// workspaceID.
	SupersedeMemory(ctx context.Context, workspaceID, oldID, newID string) error
	// ForgetMemory turns a record into a tombstone that is never listed as
	// active.
	ForgetMemory(ctx context.Context, workspaceID, id string) error
	// ApplyMemoryMutationEvent commits a memory change and its session event
	// atomically.
	ApplyMemoryMutationEvent(ctx context.Context, sid, turnID, eventType string, eventData any, m MemoryMutation) (MemoryRecord, error)
	// ListMemory returns the workspace's records matching f. It first stores
	// "expired" as the status of every record MemoryExpired reports.
	ListMemory(ctx context.Context, f MemoryFilter) ([]MemoryRecord, error)
	// ObserveMemory records that session sid saw memory id and returns the
	// number of distinct sessions that have.
	ObserveMemory(ctx context.Context, id, sid string) (int, error)
	// MemoryWatermark returns the last event sequence memory extraction has
	// processed for the session (0 if none).
	MemoryWatermark(ctx context.Context, sid string) (int, error)
	// AdvanceMemoryWatermark raises the watermark to seq; it never lowers it.
	AdvanceMemoryWatermark(ctx context.Context, sid string, seq int) error
}

// Routing holds the router's per-session decisions and prompt-cache ledger.
type Routing interface {
	// Cache returns the cache ledger entry for (sid, model), or a zero entry
	// with those fields set when none exists.
	Cache(ctx context.Context, sid, model string) (CacheEntry, error)
	WarmCache(ctx context.Context, sid, model string, tokens int, ttl time.Duration) error
	WriteRouting(ctx context.Context, r RoutingRecord) error
	UpdateRoutingOutcome(ctx context.Context, id, field string, v any) error
	// UpdateSessionRoutingOutcome sets the session outcome on all of the
	// session's routing records.
	UpdateSessionRoutingOutcome(ctx context.Context, sid string, v any) error
	// UpdateLatestTurnRoutingOutcome sets the turn outcome on the newest
	// "turn" or "escalation" record for the turn.
	UpdateLatestTurnRoutingOutcome(ctx context.Context, sid string, turn int, v any) error
	// UpdateLatestJobRoutingOutcome sets the job outcome on the newest "spawn"
	// or "review" record that has none.
	UpdateLatestJobRoutingOutcome(ctx context.Context, sid string, v any) error
}

// RouteStats is telemetry about model routes shared by every session, which
// the router uses to score routes. It is not per-session state: a host
// running many agents can back it with one shared implementation and
// delegate to it from each agent's Store.
type RouteStats interface {
	// RecordModelOutcomes adds each delta's counts to its route's totals.
	// The engine derives the deltas from session events.
	RecordModelOutcomes(ctx context.Context, deltas []ModelOutcome) error
	RecordModelCall(ctx context.Context, route string, latency time.Duration, outputTokens int, truncated bool, inputTokens, cacheReadTokens int) error
	RecordModelFailure(ctx context.Context, route, kind string) error
	ModelStats(ctx context.Context) ([]ModelStat, error)
	// ModelOutcomes returns per-route outcome stats ordered by route.
	ModelOutcomes(ctx context.Context) ([]ModelOutcome, error)
}

// Pinger is implemented by stores that can report their health.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Archiver is implemented by stores that can archive and measure sessions.
// The engine's retention uses it when present.
type Archiver interface {
	// ArchiveSession atomically replaces everything recorded for a finished
	// session except its session row — events, messages, receipts, todos,
	// work items, continuation, pending inputs, queued messages, routing
	// records, cache ledger, checkpoints, memory watermark — with one
	// compressed archive, and sets the session's ArchivedAt. The durable
	// summary stays on the session row. Archiving an archived session is a
	// no-op. The store does not judge whether the session is finished; the
	// caller does.
	//
	// The archive keeps what SessionArchive holds. Receipts, queued
	// messages, routing records and the cache ledger are dropped without
	// being archived: they only matter while the session can still run. The
	// session's jobs are left in place, since they are small and are read
	// for finished sessions (job status, the jobs listing); they are not
	// part of the archive.
	ArchiveSession(ctx context.Context, sid string) error
	// ReadArchive returns what ArchiveSession removed, or an error matching
	// ErrNotFound when the session has no archive.
	ReadArchive(ctx context.Context, sid string) (SessionArchive, error)
	// UsageBytes reports the store's current size in bytes, counting space
	// that archiving or deleting sessions has freed as free.
	UsageBytes(ctx context.Context) (int64, error)
}

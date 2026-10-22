package store

import "github.com/ductone/orrery/storage"

// The record types are defined in the public storage package; these aliases
// keep existing references to store.X working.
type (
	Session        = storage.Session
	Message        = storage.Message
	Checkpoint     = storage.Checkpoint
	PendingInput   = storage.PendingInput
	QueuedMessage  = storage.QueuedMessage
	Todo           = storage.Todo
	WorkItem       = storage.WorkItem
	Continuation   = storage.Continuation
	Job            = storage.Job
	SessionArchive = storage.SessionArchive
	Event          = storage.Event
	RequestReceipt = storage.RequestReceipt
	CacheEntry     = storage.CacheEntry
	RoutingRecord  = storage.RoutingRecord
	Workspace      = storage.Workspace
	MemoryRecord   = storage.MemoryRecord
	MemoryFilter   = storage.MemoryFilter
	MemoryMutation = storage.MemoryMutation
	ModelOutcome   = storage.ModelOutcome
	ModelStat      = storage.ModelStat
)

var (
	ErrMemoryNotFound        = storage.ErrMemoryNotFound
	ErrMemoryNotSupersedable = storage.ErrMemoryNotSupersedable
	MemoryKinds              = storage.MemoryKinds
	MemoryProvenances        = storage.MemoryProvenances
)

const (
	MaxFallbackTitle  = storage.MaxFallbackTitle
	MaxGeneratedTitle = storage.MaxGeneratedTitle
	TitlePrompt       = storage.TitlePrompt
	TodoPending       = storage.TodoPending
	TodoInProgress    = storage.TodoInProgress
	TodoCompleted     = storage.TodoCompleted
	TodoDropped       = storage.TodoDropped
)

// TodoOpen reports whether a todo status is pending or in_progress.
func TodoOpen(status string) bool { return storage.TodoOpen(status) }

// OpenTodos returns the text of each pending or in-progress todo.
func OpenTodos(ts []Todo) []string { return storage.OpenTodos(ts) }

// SessionTitle returns a deterministic fallback title for a session spec.
func SessionTitle(spec string) string { return storage.SessionTitle(spec) }

// NormalizeTitle validates and cleans a model-generated title.
func NormalizeTitle(s string) string { return storage.NormalizeTitle(s) }

var (
	_ storage.Store    = (*Store)(nil)
	_ storage.Archiver = (*Store)(nil)
)

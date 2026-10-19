package core

// Option configures an Engine at construction; see New.
type Option func(*Engine)

// WithScratchRoot makes the engine keep session scratch directories under
// dir (dir/<sessionID>) instead of the process-wide default root. The engine
// owns dir exclusively: its startup cleanup removes any directory there that
// is old enough and not a running session of this engine's store, including
// directories its store has never heard of. Give each engine its own
// directory, and do not point two engines at one.
func WithScratchRoot(dir string) Option {
	return func(e *Engine) { e.scratchRoot, e.scratchOwned = dir, dir != "" }
}

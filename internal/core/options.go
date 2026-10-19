package core

import "log/slog"

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

// WithLogger sends the engine's log output (and its tools') to l instead of
// slog.Default(). A host running many engines passes one per engine, with
// attributes such as the agent ID.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) { e.logger = l }
}

// WithProfile sets the domain part of the engine's system prompt, replacing
// the default CodingProfile. The engine's own harness rules are always
// included.
func WithProfile(p Profile) Option {
	return func(e *Engine) { e.profile = p }
}

// WithTools offers host-provided tools to the model; see Tool. It panics on a
// tool with no name or handler, a name used twice, or a name a built-in tool
// uses (ValidateTools reports these as an error instead).
func WithTools(tools ...Tool) Option {
	if err := ValidateTools(tools); err != nil {
		panic(err)
	}
	return func(e *Engine) { e.hostTools = append(e.hostTools, tools...) }
}

// WithToolFilter keeps only the tools for which allow returns true, among
// every tool offered to the model: file tools, engine tools, MCP tools and
// host tools. Filtered tools are absent from the request and the system
// prompt, and cannot be called. Several filters all must allow a tool.
func WithToolFilter(allow func(name string) bool) Option {
	return func(e *Engine) {
		if allow != nil {
			e.toolFilters = append(e.toolFilters, allow)
		}
	}
}

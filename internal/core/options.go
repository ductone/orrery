package core

import (
	"context"
	"log/slog"

	"github.com/ductone/orrery/agentproto"
	"github.com/google/uuid"
)

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

// WithMemorySelector makes the engine ask sel which memory records to
// inject into a session's prompt instead of using the built-in ranking; nil
// keeps the default. See MemorySelector.
func WithMemorySelector(sel MemorySelector) Option {
	return func(e *Engine) { e.memorySelector = sel }
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

// InputHandler answers a session's request for input without a person. It
// returns the answer and true to continue immediately, or false to pause as
// usual so the answer can come later through Continue. An error pauses too,
// and is logged. The request's Kind says what is being asked.
type InputHandler func(ctx context.Context, sessionID string, input agentproto.InputRequest) (answer string, ok bool, err error)

// DefaultMaxAutoAnswers is how many times in one run an InputHandler may
// answer before the session pauses for a person regardless.
const DefaultMaxAutoAnswers = 10

// WithInputHandler makes the engine ask h before pausing a session for input.
// An answer is applied as if a person had given it through Continue, and Run
// (or the channel of a Start or Continue) reports the outcome of the continued
// session, not the pause. h may be called from any session's goroutine.
func WithInputHandler(h InputHandler) Option {
	return func(e *Engine) { e.inputHandler = h }
}

// WithMaxAutoAnswers bounds how many answers the InputHandler gives in one run
// (a run is the chain of turns an answer starts from one Run, Start or
// Continue): after that the session pauses for a person. It stops a handler
// that agrees to everything from looping forever. n <= 0 means
// DefaultMaxAutoAnswers.
func WithMaxAutoAnswers(n int) Option {
	return func(e *Engine) { e.maxAutoAnswers = n }
}

type autoAnswersKey struct{}

// runAnswering runs a root session turn and, when it pauses for input and the
// engine has an InputHandler that answers, applies the answer through the
// normal Continue path and returns the result of the continued run.
//
// The answer is applied by ContinueIntegrated itself rather than in the run
// loop, so validation, the budget extension, the harness note and the
// latest-request rule are exactly the person's path. The loop has returned by
// then (the pause is durable, the workspace lock released), so the continue
// starts a fresh turn; its goroutine runs this same wrapper, and the count of
// answers so far travels in the context to bound the chain.
func (e *Engine) runAnswering(ctx context.Context, id string, req agentproto.TaskRequest, emit EmitFunc) agentproto.TaskResult {
	result := e.runRoot(ctx, id, req, emit)
	if e.inputHandler == nil || result.Status != agentproto.InputRequired || ctx.Err() != nil {
		return result
	}
	input, ok := result.Result["input"].(agentproto.InputRequest)
	if !ok {
		return result
	}
	limit := e.maxAutoAnswers
	if limit <= 0 {
		limit = DefaultMaxAutoAnswers
	}
	answered, _ := ctx.Value(autoAnswersKey{}).(int)
	if answered >= limit {
		e.log().Warn("input handler answer limit reached; pausing for a person", "session", id, "limit", limit)
		return result
	}
	answer, ok, err := e.inputHandler(ctx, id, input)
	if err != nil {
		e.log().Warn("input handler failed; pausing for a person", "session", id, "input", input.ID, "kind", input.Kind, "error", err)
		return result
	}
	if !ok {
		return result
	}
	// The turn is over: let the continue start the next one. Keep the
	// context's values but not its deadline, which belonged to the turn.
	e.mu.Lock()
	delete(e.cancels, id)
	delete(e.turnIDs, id)
	e.mu.Unlock()
	next := context.WithValue(context.WithoutCancel(ctx), autoAnswersKey{}, answered+1)
	info, err := e.ContinueIntegrated(next, id, answer, uuid.NewString(), "input-handler", emit)
	if err != nil || info.Result == nil {
		e.log().Warn("input handler's answer was not accepted; pausing for a person", "session", id, "input", input.ID, "kind", input.Kind, "error", err)
		return result
	}
	return <-info.Result
}

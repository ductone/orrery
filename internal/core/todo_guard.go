package core

import (
	"context"
	"strings"

	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
)

// maxTodoRejections bounds how often one run refuses a final answer for open
// todo items. After that the answer is accepted with the open items recorded,
// so a plan the agent will not update cannot burn the budget in a loop.
const maxTodoRejections = 2

// OpenTodos returns the text of each todo in a session's plan that is still
// pending or in progress. Hosts use it to apply the same rule to their own
// ways of finishing (a host tool that ends a run, say).
func (e *Engine) OpenTodos(ctx context.Context, sid string) ([]string, error) {
	ts, err := e.store.Todos(ctx, sid)
	if err != nil {
		return nil, err
	}
	return store.OpenTodos(ts), nil
}

// OpenTodosMessage is the harness's request to close a plan before finishing.
func OpenTodosMessage(open []string) string {
	return "Your todo plan still has open items:\n- " + strings.Join(open, "\n- ") + "\nDo them, or update the plan with todo (mark an item completed, or dropped if you decided it is not needed) before finishing."
}

// openTodosGuard refuses a root session's final answer while its todo plan has
// pending or in-progress items. It reports true when the answer was refused
// and the turn loop should continue. Workers are not guarded: their result
// goes to the parent, which judges it. Nor is a turn that cannot call todo,
// since the agent could not close the plan.
func (e *Engine) openTodosGuard(ctx context.Context, sid, parentJob string, canUpdate bool, progress *progressTracker, emit EmitFunc) bool {
	if parentJob != "" || !canUpdate {
		return false
	}
	open, err := e.OpenTodos(ctx, sid)
	if err != nil || len(open) == 0 {
		return false
	}
	if progress.todoRejections >= maxTodoRejections {
		e.emit(ctx, sid, "completion.accepted_open_todos", map[string]any{"open": open, "rejections": progress.todoRejections}, emit)
		return false
	}
	progress.todoRejections++
	progress.completionRejections++
	e.emit(ctx, sid, "completion.rejected", map[string]any{"reason": "open todo items", "open": open, "attempt": progress.todoRejections}, emit)
	_ = e.store.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Harness: true, Content: OpenTodosMessage(open)})
	return true
}

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"

	builtin "github.com/ductone/orrery/internal/tools"
)

// Tool is a host-provided tool offered to the model.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema object
	// Handler runs the tool for a session. Its result is marshaled to JSON for
	// the model; an error is reported to the model as a failed call. A panic
	// is recovered and reported the same way.
	Handler func(ctx context.Context, call ToolCall) (any, error)
	// ReadOnly tools are also offered to read-only workers.
	ReadOnly bool
	// Workers controls whether worker (child) sessions get the tool; the
	// default, false, offers it to top-level sessions only.
	Workers bool
}

// ToolCall is one invocation of a host Tool.
type ToolCall struct {
	SessionID string
	Args      map[string]any
}

// reservedToolNames are the names the engine's own tools can take in some
// session: file tools, engine tools, and the optional web_search and lsp.
var reservedToolNames = []string{
	"read", "search", "edit", "write", "replace", "ast_edit", "exec", "job",
	"memory", "ask", "recall_history", "todo", "artifact", "link", "consult",
	"spawn", "job_result", "skill", "web_search", "fetch", "lsp",
}

// ValidateTools reports a host tool that has no name or handler, or
// a name another host tool or a built-in tool already uses. (A name that an
// MCP server's tool also uses is not detectable here; the host tool wins.)
func ValidateTools(tools []Tool) error {
	seen := map[string]bool{}
	for _, t := range tools {
		switch {
		case t.Name == "":
			return errors.New("host tool has no name")
		case t.Handler == nil:
			return fmt.Errorf("host tool %q has no handler", t.Name)
		case slices.Contains(reservedToolNames, t.Name):
			return fmt.Errorf("host tool %q collides with a built-in tool", t.Name)
		case seen[t.Name]:
			return fmt.Errorf("host tool %q is defined twice", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// addHostTools registers the host's tools that this session may use.
func (e *Engine) addHostTools(r *builtin.Registry, sid, parentJob string, readMode bool) {
	for _, t := range e.hostTools {
		if (readMode && !t.ReadOnly) || (parentJob != "" && !t.Workers) {
			continue
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		handler := t.Handler
		r.Add(t.Name, t.Description, schema, func(ctx context.Context, a map[string]any) (any, error) {
			return handler(ctx, ToolCall{SessionID: sid, Args: a})
		})
	}
}

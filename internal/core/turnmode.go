package core

import (
	"slices"
	"strings"

	"github.com/ductone/orrey/internal/provider"
)

// turnMode is how a forced intervention constrains one turn. Constraints never
// change the request's stable prefix: tool definitions stay as they are, and
// directives travel as a trailing user message that is not persisted. Swapping
// tool definitions or the system prompt instead invalidated the whole cached
// prefix on exactly the turns that were meant to be cheap, and left the model
// with tool-use history for tools it could no longer see.
type turnMode struct {
	directives []string
	// allowed limits which tools may be called; nil allows every tool.
	allowed []string
	noCalls bool
}

// restrict adds a directive and replaces the tool constraint. Passing no tools
// forbids tool calls entirely. The last restriction wins, matching the order
// in which interventions are considered.
func (m *turnMode) restrict(directive string, tools ...string) {
	m.directives = append(m.directives, directive)
	m.noCalls = len(tools) == 0
	m.allowed = tools
}

// advise adds a directive without constraining tools. "Finish now" advice
// leaves tools available: turning them off once trapped an agent that had
// review findings to fix.
func (m *turnMode) advise(directive string) {
	m.directives = append(m.directives, directive)
}

func (m *turnMode) active() bool { return len(m.directives) > 0 }

// permits reports whether a tool call is allowed this turn.
func (m *turnMode) permits(tool string) bool {
	if m.noCalls {
		return false
	}
	return m.allowed == nil || slices.Contains(m.allowed, tool)
}

// unavailable explains a refused call in the tool result.
func (m *turnMode) unavailable(tool string) string {
	if m.noCalls {
		return "tool " + tool + " is unavailable this turn: no tool calls are allowed. Return the final result as text now."
	}
	return "tool " + tool + " is unavailable this turn; allowed tools: " + strings.Join(m.allowed, ", ") + "."
}

// apply appends the turn's directive to the provider history.
func (m *turnMode) apply(history []provider.Message) []provider.Message {
	if !m.active() {
		return history
	}
	text := "HARNESS DIRECTIVE FOR THIS TURN\n" + strings.Join(m.directives, "\n")
	if m.noCalls {
		text += "\nTool calls are disabled for this turn. Respond with text only."
	} else if m.allowed != nil {
		text += "\nOnly these tools may be called this turn: " + strings.Join(m.allowed, ", ") + "."
	}
	return append(history, provider.Message{Role: "user", Content: text})
}

const (
	defaultOutputCap = 8_000
	// maxOutputCap bounds how far truncation retries raise the output limit.
	maxOutputCap = 32_000
)

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

func emptyFinalResponse(m provider.Message) bool {
	return len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == ""
}

func compactionKeepIndex(msgs []store.Message, turnsToKeep int) int {
	latest := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		var parsed provider.Message
		_ = json.Unmarshal([]byte(msgs[i].ContentJSON), &parsed)
		if msgs[i].Role == "user" && !parsed.Harness {
			latest = i
			break
		}
	}
	kept := 0
	for i := latest; i < len(msgs); i++ {
		kept += len(msgs[i].ContentJSON)
		if kept > 40_000 {
			latest = -1
			break
		}
	}
	if latest > 0 {
		return latest
	}
	turns := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			turns++
			if turns == turnsToKeep {
				return i
			}
		}
	}
	return len(msgs)
}
func messagesText(ms []store.Message) string {
	var b strings.Builder
	for _, m := range ms {
		b.WriteString(m.ContentJSON)
	}
	return b.String()
}
func estimate(s string) int { return max(1, len(s)/4) }
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
func setPlanSnapshot(summary, plan string) string {
	const begin = "\n[TODO SNAPSHOT]\n"
	const end = "\n[/TODO SNAPSHOT]"
	if i := strings.Index(summary, begin); i >= 0 {
		if j := strings.Index(summary[i+len(begin):], end); j >= 0 {
			summary = summary[:i] + summary[i+len(begin)+j+len(end):]
		}
	}
	return summary + begin + plan + end
}
func parseResult(s string) map[string]any {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
	s = strings.TrimSuffix(s, "```")
	var v map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &v) == nil {
		return v
	}
	// Reasoning models sometimes explain their verdict before emitting the
	// requested JSON object. Keep the last valid object so examples in the
	// explanation do not override the terminal structured result.
	var last map[string]any
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		var candidate map[string]any
		if json.NewDecoder(strings.NewReader(s[i:])).Decode(&candidate) == nil && candidate != nil {
			last = candidate
		}
	}
	if last != nil {
		return last
	}
	return map[string]any{"answer": s}
}
func validateSchema(schema, result map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	if err = c.AddResource("result.json", bytes.NewReader(b)); err != nil {
		return err
	}
	compiled, err := c.Compile("result.json")
	if err != nil {
		return err
	}
	if err := compiled.Validate(result); err != nil {
		parsed, _ := json.Marshal(result)
		return fmt.Errorf("parsed result %s does not validate: %w", truncate(string(parsed), 500), err)
	}
	return nil
}

func (e *Engine) inferPhase(ctx context.Context, sid, toolName, command string, progress *progressTracker) {
	s, err := e.store.Session(ctx, sid)
	if err != nil {
		return
	}
	next := s.Phase

	// Edit while in explore/plan → implement (initial implementation work).
	if toolName == "edit" && (next == string(router.Explore) || next == string(router.Plan)) {
		next = string(router.Implement)
	}

	// Edit while in review/diagnose → implement (work resumed after review).
	if toolName == "edit" && (next == string(router.Review) || next == string(router.Diagnose)) {
		next = string(router.Implement)
	}

	cmd := strings.ToLower(command)
	if toolName == "exec" && progress.edited && containsAny(cmd, " test", "test ", "lint", "typecheck", "build", "check", "vet") {
		next = string(router.Review)
	}

	// When independent review and verification both passed, advance to wrap-up
	// so the session can complete instead of lingering in review until the bound fires.
	if next == string(router.Review) && progress.reviewed && progress.verified {
		next = string(router.WrapUp)
	}

	if next != s.Phase {
		s.Phase = next
		_ = e.store.UpdateSession(ctx, s)
	}
}

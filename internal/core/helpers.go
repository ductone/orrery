package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/router"
	"github.com/ductone/orrery/internal/store"
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

const (
	planSnapshotBegin = "\n[TODO SNAPSHOT]\n"
	planSnapshotEnd   = "\n[/TODO SNAPSHOT]"
)

// setPlanSnapshot replaces the todo snapshot appended to a durable summary.
func setPlanSnapshot(summary, plan string) string {
	rest, _ := splitPlanSnapshot(summary)
	return rest + planSnapshotBegin + plan + planSnapshotEnd
}

// splitPlanSnapshot separates a durable summary from its todo snapshot (""
// when it has none). The snapshot changes at phase boundaries, so it is
// rendered after the cached prefix (planSnapshotSection), never in it.
func splitPlanSnapshot(summary string) (rest, plan string) {
	i := strings.Index(summary, planSnapshotBegin)
	if i < 0 {
		return summary, ""
	}
	j := strings.Index(summary[i+len(planSnapshotBegin):], planSnapshotEnd)
	if j < 0 {
		return summary, ""
	}
	return summary[:i] + summary[i+len(planSnapshotBegin)+j+len(planSnapshotEnd):], summary[i+len(planSnapshotBegin) : i+len(planSnapshotBegin)+j]
}

// planSnapshotSection is the durable summary's todo snapshot as the
// trailing request message carries it, or "" when there is none.
func planSnapshotSection(summary string) string {
	if _, plan := splitPlanSnapshot(summary); plan != "" {
		return "\n\nTODO SNAPSHOT (the plan at the last phase boundary; the live todo is in tool-result history)\n" + plan
	}
	return ""
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
	if isEditTool(toolName) && (next == string(router.Explore) || next == string(router.Plan)) {
		next = string(router.Implement)
	}

	// Edit while in review/diagnose → implement (work resumed after review).
	if isEditTool(toolName) && (next == string(router.Review) || next == string(router.Diagnose)) {
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

// withoutPlanSnapshot is summary without its todo snapshot.
func withoutPlanSnapshot(summary string) string {
	rest, _ := splitPlanSnapshot(summary)
	return rest
}

// modelJSON is the JSON value in a model's response, for json.Unmarshal:
// the response itself when it is valid JSON, else the body of its ```
// (or ```json) fence, else its single top-level object or array among
// surrounding prose. Otherwise it is the trimmed response unchanged, so
// the caller's decode error stands.
func modelJSON(s string) string {
	s = strings.TrimSpace(s)
	if json.Valid([]byte(s)) {
		return s
	}
	if i := strings.Index(s, "```"); i >= 0 {
		body := s[i+3:]
		if nl := strings.IndexByte(body, '\n'); nl >= 0 && !strings.ContainsAny(body[:nl], "{[") {
			body = body[nl+1:]
		}
		if j := strings.Index(body, "```"); j >= 0 {
			body = body[:j]
		}
		if b := strings.TrimSpace(body); json.Valid([]byte(b)) {
			return b
		}
	}
	var found []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			continue
		}
		found = append(found, string(raw))
		i += int(dec.InputOffset()) - 1
	}
	if len(found) == 1 {
		return found[0]
	}
	return s
}

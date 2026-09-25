package tui

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

type blockKind uint8

const (
	blockUser blockKind = iota
	blockAssistant
	blockTool
	blockRouting
	blockWorker
	blockTerminal
	blockInput
	blockNotice
	blockPanel
)

type tone uint8

const (
	toneDim tone = iota
	toneInfo
	toneGood
	toneWarn
	toneError
)

// block is one committed transcript entry. Committed blocks are immutable and
// are printed into terminal scrollback; everything still changing lives in
// state and is drawn in the live region instead.
type block struct {
	kind       blockKind
	text       string
	reasoning  string
	structured map[string]any
	model      string
	effort     string
	icon       string
	tone       tone
	tool       *toolRecord
	result     *agentproto.TaskResult
	input      *agentproto.InputRequest
	todos      []store.Todo
	panel      func(*renderer) string
	review     bool
}

type toolRecord struct {
	call    provider.ToolCall
	result  any
	failed  bool
	dedup   bool
	elapsed time.Duration
}

type runningTool struct {
	call    provider.ToolCall
	started time.Time
}

type worker struct {
	id, model, spec, mode string
	review                bool
	started               time.Time
}

type queuedMessage struct{ requestID, content string }

// state is the session as derived from its durable event log. It is a pure
// reducer: replaying the same events always yields the same blocks and state,
// which is what lets the TUI attach to a running or finished session.
type state struct {
	status        string
	statusAt      time.Time
	turnStarted   time.Time
	model, effort string
	phase         string
	tool          *runningTool
	workers       []worker
	queued        []queuedMessage
	input         *agentproto.InputRequest
	todos         []store.Todo
	tokensIn      int
	tokensOut     int
	cacheRead     int
	cost          float64 // sum of usage events in this session
	spent         float64 // ledger spend from the last snapshot
	contextTokens int
	lastSeq       int
	lastEventAt   time.Time
	started       bool
	sawUser       bool
	lastAnswer    string
	lastResult    map[string]any
}

func (s *state) running() bool { return s.status == "running" }

// spend is the session's cost so far. The ledger also counts workers and
// judges that report usage outside this session's events; usage events can
// run ahead of a snapshot that predates them.
func (s *state) spend() float64 { return max(s.cost, s.spent) }

// apply folds one event into the state and returns the blocks it commits.
func (s *state) apply(ev Event) []block {
	if ev.Seq <= s.lastSeq {
		return nil
	}
	s.lastSeq = ev.Seq
	s.lastEventAt = ev.CreatedAt
	ev.Data = cleanJSON(ev.Data)
	switch ev.Type {
	case "turn.accepted":
		s.beginTurn(ev.CreatedAt)
	case "session.started":
		s.beginTurn(ev.CreatedAt)
		var d struct{ Spec string }
		_ = json.Unmarshal(ev.Data, &d)
		first := !s.started
		s.started = true
		if first && !s.sawUser && strings.TrimSpace(d.Spec) != "" {
			s.sawUser = true
			return []block{{kind: blockUser, text: d.Spec}}
		}
	case "user.message":
		var d struct {
			RequestID string           `json:"request_id"`
			Content   provider.Message `json:"content"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.sawUser = true
		s.dropQueued(d.RequestID)
		return []block{{kind: blockUser, text: d.Content.Content}}
	case "message.queued":
		var d struct {
			RequestID string `json:"request_id"`
			Content   string `json:"content"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.queued = append(s.queued, queuedMessage{requestID: d.RequestID, content: d.Content})
	case "routing.decision", "routing.fallback":
		var d struct {
			Decision struct {
				Model     struct{ ID string }
				Effort    string `json:"effort"`
				WasSwitch bool   `json:"was_switch"`
			} `json:"decision"`
			Explanation string `json:"explanation"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.Decision.Model.ID != "" {
			s.model = d.Decision.Model.ID
		}
		s.effort = d.Decision.Effort
		b := block{kind: blockRouting, text: d.Explanation, model: d.Decision.Model.ID, effort: d.Decision.Effort, tone: toneDim}
		if ev.Type == "routing.fallback" {
			b.tone = toneWarn
		}
		return []block{b}
	case "routing.retry":
		var d struct {
			Model   string
			Attempt int
		}
		_ = json.Unmarshal(ev.Data, &d)
		return []block{notice("↻", toneWarn, "retrying %s (attempt %d)", shortModel(d.Model), d.Attempt)}
	case "assistant.message":
		var d struct {
			Message provider.Message `json:"message"`
			Model   string           `json:"model"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		content := strings.TrimSpace(d.Message.Content)
		if content == "" && strings.TrimSpace(d.Message.Reasoning) == "" {
			return nil
		}
		b := block{kind: blockAssistant, text: content, reasoning: strings.TrimSpace(d.Message.Reasoning), model: d.Model}
		if structured := structuredContent(content); structured != nil {
			b.structured = structured
			s.lastResult = structured
		}
		if content != "" {
			s.lastAnswer = content
		}
		return []block{b}
	case "usage.reported":
		var d struct {
			Model            string  `json:"model"`
			JobID            string  `json:"job_id"`
			Kind             string  `json:"kind"`
			InputTokens      int     `json:"input_tokens"`
			OutputTokens     int     `json:"output_tokens"`
			CacheReadTokens  int     `json:"cache_read_tokens"`
			CacheWriteTokens int     `json:"cache_write_tokens"`
			CostUSD          float64 `json:"cost_usd"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.tokensIn += d.InputTokens
		s.tokensOut += d.OutputTokens
		s.cacheRead += d.CacheReadTokens
		s.cost += d.CostUSD
		if d.JobID == "" && d.Kind == "" {
			s.contextTokens = contextTokens(d.Model, d.InputTokens, d.CacheReadTokens, d.CacheWriteTokens)
		}
	case "tool.started":
		var call provider.ToolCall
		_ = json.Unmarshal(ev.Data, &call)
		s.tool = &runningTool{call: call, started: ev.CreatedAt}
	case "tool.finished":
		var d struct {
			Call         provider.ToolCall `json:"call"`
			Result       any               `json:"result"`
			Deduplicated bool              `json:"deduplicated"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		rec := &toolRecord{call: d.Call, result: d.Result, dedup: d.Deduplicated, failed: toolFailed(d.Result)}
		if s.tool != nil && s.tool.call.ID == d.Call.ID {
			rec.elapsed = ev.CreatedAt.Sub(s.tool.started)
		}
		s.tool = nil
		b := block{kind: blockTool, tool: rec}
		if d.Call.Name == "todo" {
			b.todos = append([]store.Todo(nil), s.todos...)
		}
		return []block{b}
	case "todo.changed":
		var todos []store.Todo
		_ = json.Unmarshal(ev.Data, &todos)
		s.todos = todos
		for _, t := range todos {
			if t.Status == "in_progress" {
				s.phase = t.Phase
				break
			}
		}
	case "job.started":
		var d struct {
			ID            string `json:"id"`
			Spec          string `json:"spec"`
			Model         string `json:"model"`
			WorkspaceMode string `json:"workspace_mode"`
			Review        bool   `json:"review"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.workers = append(s.workers, worker{id: d.ID, model: d.Model, spec: d.Spec, mode: d.WorkspaceMode, review: d.Review, started: ev.CreatedAt})
		return []block{{kind: blockWorker, text: d.Spec, model: d.Model, effort: d.WorkspaceMode, review: d.Review}}
	case "job.terminal":
		var d struct {
			ID     string                `json:"id"`
			Result agentproto.TaskResult `json:"result"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		b := block{kind: blockWorker, result: &d.Result}
		for i, w := range s.workers {
			if w.id == d.ID {
				b.text, b.model, b.review = w.spec, w.model, w.review
				s.workers = append(s.workers[:i], s.workers[i+1:]...)
				break
			}
		}
		return []block{b}
	case "input.required":
		var in agentproto.InputRequest
		_ = json.Unmarshal(ev.Data, &in)
		s.setStatus("input_required", ev.CreatedAt)
		s.input = &in
		s.tool = nil
		return []block{{kind: blockInput, input: &in}}
	case "input.answered":
		s.input = nil
	case "session.terminal":
		var r agentproto.TaskResult
		_ = json.Unmarshal(ev.Data, &r)
		s.setStatus(string(r.Status), ev.CreatedAt)
		s.tool = nil
		b := block{kind: blockTerminal, result: &r}
		if r.Result != nil {
			if _, plain := r.Result["answer"]; plain && len(r.Result) == 1 {
				b.structured = nil
			} else if !reflect.DeepEqual(r.Result, s.lastResult) {
				b.structured = r.Result
			}
		}
		s.lastResult = nil
		return []block{b}
	case "session.terminated":
		s.setStatus("terminated", ev.CreatedAt)
		s.tool = nil
		return []block{notice("■", toneWarn, "session terminated")}
	case "session.restored":
		var d struct {
			CheckpointID string `json:"checkpoint_id"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.contextTokens = 0
		return []block{notice("↶", toneInfo, "restored checkpoint %s", shortID(d.CheckpointID))}
	case "context.compacted":
		var d struct {
			KeptTurns any    `json:"kept_turns"`
			Reason    string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.contextTokens = 0
		text := "context compacted"
		if d.Reason != "" {
			text += " · " + d.Reason
		}
		if d.KeptTurns != nil {
			text += " · " + formatAny(d.KeptTurns) + " turns kept"
		}
		return []block{notice("↺", toneInfo, "%s", text)}
	case "context.compaction_failed", "runtime_config.reload_failed":
		var d struct{ Error string }
		_ = json.Unmarshal(ev.Data, &d)
		return []block{notice("⚠", toneWarn, "%s: %s", strings.ReplaceAll(ev.Type, "_", " "), d.Error)}
	case "budget.increased":
		var d struct {
			AddedUSD  float64 `json:"added_usd"`
			BudgetUSD float64 `json:"budget_usd"`
			SpentUSD  float64 `json:"spent_usd"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		return []block{notice("$", toneGood, "budget raised by %s · %s of %s spent", usd(d.AddedUSD), usd(d.SpentUSD), usd(d.BudgetUSD))}
	case "progress.intervention":
		var d struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		text := strings.ReplaceAll(d.Kind, "_", " ")
		if detail := firstNonEmpty(d.Reason, d.Error); detail != "" {
			text += ": " + detail
		}
		return []block{notice("◎", toneWarn, "%s", text)}
	case "progress.judge":
		var d struct {
			Kind      string `json:"kind"`
			Intervene *bool  `json:"intervene"`
			Reason    string `json:"reason"`
			Error     string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		text := "progress judge"
		if d.Kind != "" {
			text += " · " + strings.ReplaceAll(d.Kind, "_", " ")
		}
		if d.Intervene != nil {
			if *d.Intervene {
				text += " · intervene"
			} else {
				text += " · keep going"
			}
		}
		if detail := firstNonEmpty(d.Reason, d.Error); detail != "" {
			text += ": " + detail
		}
		return []block{notice("◎", toneDim, "%s", text)}
	case "completion.rejected":
		var d struct {
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		text := "completion rejected: " + d.Reason
		if d.Error != "" {
			text += " (" + d.Error + ")"
		}
		return []block{notice("↩", toneWarn, "%s", text)}
	case "provider.error":
		var d struct {
			Model string `json:"model"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.Model != "" {
			return []block{notice("✗", toneError, "%s: %s", shortModel(d.Model), d.Error)}
		}
		return []block{notice("✗", toneError, "%s", d.Error)}
	case "instruction.discovered":
		var d struct {
			Documents []struct {
				Path string `json:"path"`
			} `json:"documents"`
			Blocked bool `json:"blocked"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		paths := make([]string, 0, len(d.Documents))
		for _, doc := range d.Documents {
			paths = append(paths, doc.Path)
		}
		text := "instructions discovered: " + strings.Join(paths, ", ")
		if d.Blocked {
			text += " · edit paused to apply them"
		}
		return []block{notice("§", toneInfo, "%s", text)}
	case "instruction.blocked":
		return []block{notice("§", toneWarn, "edit paused: instruction boundary crossed earlier in the response")}
	case "artifact.created":
		var d struct{ Path, Description string }
		_ = json.Unmarshal(ev.Data, &d)
		return []block{notice("◈", toneInfo, "artifact %s", joinNonEmpty(" · ", d.Path, d.Description))}
	case "link.created":
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		return []block{notice("↗", toneInfo, "link %s", joinNonEmpty(" · ", formatAny(pick(d, "label", "Label", "title")), formatAny(pick(d, "url", "URL"))))}
	}
	return nil
}

func (s *state) beginTurn(at time.Time) {
	if !s.running() || s.turnStarted.IsZero() {
		s.turnStarted = at
	}
	s.setStatus("running", at)
	s.input = nil
}

func (s *state) setStatus(status string, at time.Time) {
	s.status = status
	s.statusAt = at
}

func (s *state) dropQueued(requestID string) {
	for i, q := range s.queued {
		if q.requestID == requestID {
			s.queued = append(s.queued[:i], s.queued[i+1:]...)
			return
		}
	}
}

// reconcile folds a session snapshot into the derived state. The event log
// is authoritative for transitions, but a turn can end without a terminal
// event (process restart marks it interrupted), so a snapshot written after
// the newest event may override a stale running status.
func (s *state) reconcile(sess store.Session) {
	if len(s.todos) == 0 && sess.Phase != "" {
		s.phase = sess.Phase
	}
	if s.model == "" && sess.Model != "" {
		s.model = sess.Model
	}
	s.spent = sess.SpentUSD
	if sess.Status == "" || sess.Status == s.status {
		return
	}
	if s.status == "" || (sess.UpdatedAt.After(s.lastEventAt) && sess.UpdatedAt.After(s.statusAt)) {
		s.setStatus(sess.Status, sess.UpdatedAt)
		s.turnStarted = time.Time{}
		if !s.running() {
			s.tool = nil
		}
	}
}

func notice(icon string, t tone, format string, args ...any) block {
	return block{kind: blockNotice, icon: icon, tone: t, text: sprintf(format, args...)}
}

// structuredContent returns the JSON object when a final assistant message
// is a structured completion, so it can be rendered as sections, not JSON.
func structuredContent(content string) map[string]any {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	return v
}

func toolFailed(result any) bool {
	m, ok := result.(map[string]any)
	if !ok {
		return false
	}
	if e, ok := m["error"]; ok && e != nil && e != "" {
		return true
	}
	if okv, ok := m["ok"].(bool); ok && !okv {
		return true
	}
	if blocked, ok := m["blocked"].(bool); ok && blocked {
		return true
	}
	return false
}

// contextTokens estimates the prompt size of the last root call. Anthropic
// reports cache reads and writes separately from input; OpenAI-compatible
// providers include cached tokens in the input count.
func contextTokens(model string, input, cacheRead, cacheWrite int) int {
	if strings.HasPrefix(model, "anthropic/") {
		return input + cacheRead + cacheWrite
	}
	return input
}

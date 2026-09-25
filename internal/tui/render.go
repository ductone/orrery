package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/glamour/v2"
	mdstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/store"
)

const (
	collapsedLines = 6
	expandedLines  = 400
)

// renderer turns committed blocks into styled text at a fixed width. Output
// lines never exceed the width, so scrollback insertion stays exact.
type renderer struct {
	width    int
	st       styles
	expand   bool
	thinking bool
	md       *glamour.TermRenderer
	mdWidth  int
}

func newRenderer() *renderer { return &renderer{width: 80, st: newStyles()} }

func (r *renderer) setWidth(w int) {
	if w < 20 {
		w = 20
	}
	r.width = w
}

func (r *renderer) markdown(text string) string {
	w := r.width - 2
	if r.md == nil || r.mdWidth != w {
		cfg := mdstyles.DarkStyleConfig
		zero := uint(0)
		cfg.Document.Margin = &zero
		cfg.Document.BlockPrefix = ""
		cfg.Document.BlockSuffix = ""
		cfg.Document.Color = strPtr("#e6e8eb")
		cfg.H1.BackgroundColor = nil
		cfg.H1.Color = strPtr("#00ffcc")
		cfg.H1.Prefix = "# "
		cfg.H1.Suffix = ""
		cfg.Link.Color = strPtr("#9b80f8")
		cfg.Code.BackgroundColor = strPtr("#2a2e35")
		cfg.Code.Color = strPtr("#00ffcc")
		md, err := glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(w), glamour.WithPreservedNewLines())
		if err != nil {
			return r.wrap(text, w)
		}
		r.md, r.mdWidth = md, w
	}
	out, err := r.md.Render(text)
	if err != nil {
		return r.wrap(text, w)
	}
	return strings.Trim(out, "\n")
}

func strPtr(s string) *string { return &s }

func (r *renderer) wrap(text string, width int) string {
	if width < 4 {
		width = 4
	}
	return ansi.Wrap(text, width, "")
}

// indent prefixes every line of body. The first line uses first, the rest use
// rest; both must have the same display width.
func indent(body, first, rest string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = first + l
		} else {
			lines[i] = rest + l
		}
	}
	return strings.Join(lines, "\n")
}

func (r *renderer) toneStyle(t tone) lipgloss.Style {
	switch t {
	case toneInfo:
		return r.st.signal
	case toneGood:
		return r.st.accent
	case toneWarn:
		return r.st.warning
	case toneError:
		return r.st.danger
	default:
		return r.st.muted
	}
}

// compact reports whether a block is a one-line annotation that may sit
// directly under another annotation without a separating blank line.
func compact(b block) bool {
	return b.kind == blockRouting || b.kind == blockNotice
}

func (r *renderer) render(b block) string {
	switch b.kind {
	case blockUser:
		return r.user(b.text)
	case blockAssistant:
		return r.assistant(b)
	case blockTool:
		return r.tool(b)
	case blockRouting:
		return r.routing(b)
	case blockWorker:
		return r.worker(b)
	case blockTerminal:
		return r.terminal(b)
	case blockInput:
		return r.inputBlock(b.input)
	case blockPanel:
		return b.panel(r)
	default:
		return r.notice(b)
	}
}

func (r *renderer) user(text string) string {
	w := r.width - 3
	var out []string
	for _, line := range strings.Split(r.wrap(strings.TrimRight(text, "\n"), w), "\n") {
		pad := w - ansi.StringWidth(line)
		if pad < 0 {
			pad = 0
		}
		out = append(out, r.st.userBar.Render("▌")+r.st.userText.Render(" "+line+strings.Repeat(" ", pad)))
	}
	return strings.Join(out, "\n")
}

func (r *renderer) assistant(b block) string {
	var parts []string
	if b.reasoning != "" {
		if r.thinking {
			parts = append(parts, indent(r.st.italic.Render(r.wrap(b.reasoning, r.width-4)), " ∴ ", "   "))
		} else {
			parts = append(parts, r.st.italic.Render(fmt.Sprintf(" ∴ thinking · %s chars", tokens(len(b.reasoning)))))
		}
	}
	switch {
	case b.structured != nil:
		parts = append(parts, r.structured(b.structured))
	case b.text != "":
		parts = append(parts, indent(r.markdown(b.text), " ", " "))
	}
	return strings.Join(parts, "\n")
}

func (r *renderer) routing(b block) string {
	style := r.st.muted
	icon := "↗"
	if b.tone == toneWarn {
		style, icon = r.st.warning, "⇄"
	}
	text := b.text
	if text == "" {
		text = "routed to " + shortModel(b.model)
	}
	if b.effort != "" && !strings.Contains(text, string(b.effort)+" effort") {
		text += " · " + b.effort + " effort"
	}
	if r.expand {
		return indent(style.Render(r.wrap(text, r.width-4)), " "+style.Render(icon)+" ", "   ")
	}
	return " " + style.Render(icon+" "+ansi.Truncate(text, r.width-4, "…"))
}

func (r *renderer) notice(b block) string {
	style := r.toneStyle(b.tone)
	return indent(style.Render(r.wrap(b.text, r.width-4)), " "+style.Render(b.icon)+" ", "   ")
}

// header lays out "icon name arg ........ right" on one line.
func (r *renderer) header(icon lipgloss.Style, glyph, name, arg, right string) string {
	left := " " + icon.Render(glyph) + " " + r.st.toolName.Render(name)
	used := ansi.StringWidth(left) + ansi.StringWidth(right) + 2
	if arg != "" && r.width-used > 4 {
		left += "  " + r.st.toolArg.Render(ansi.Truncate(strings.ReplaceAll(arg, "\n", " ⏎ "), r.width-used-2, "…"))
	}
	if right == "" {
		return left
	}
	gap := r.width - ansi.StringWidth(left) - ansi.StringWidth(right) - 1
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + r.st.muted.Render(right)
}

func (r *renderer) tool(b block) string {
	t := b.tool
	icon, glyph := r.st.accent, "✓"
	switch {
	case t.dedup:
		icon, glyph = r.st.muted, "↺"
	case t.failed:
		icon, glyph = r.st.danger, "✗"
	}
	arg := toolArgSummary(t.call.Name, t.call.Arguments)
	out := []string{r.header(icon, glyph, t.call.Name, arg, duration(t.elapsed))}
	if t.dedup {
		return out[0] + r.st.muted.Render("  duplicate call")
	}
	body, fromTail := r.toolBody(b)
	if len(body) == 0 {
		return out[0]
	}
	limit := collapsedLines
	if r.expand {
		limit = expandedLines
	}
	hidden := 0
	if len(body) > limit {
		hidden = len(body) - limit
		if fromTail {
			body = body[len(body)-limit:]
		} else {
			body = body[:limit]
		}
	}
	gutter := r.st.gutter.Render("   │ ")
	more := ""
	if hidden > 0 {
		more = gutter + r.st.muted.Render(fmt.Sprintf("… %s %s", plural(hidden, "more line"), map[bool]string{true: "", false: "· ctrl+o to expand"}[r.expand]))
	}
	if more != "" && fromTail {
		out = append(out, more)
	}
	for _, line := range body {
		out = append(out, gutter+ansi.Truncate(line, r.width-6, "…"))
	}
	if more != "" && !fromTail {
		out = append(out, more)
	}
	return strings.Join(out, "\n")
}

// toolBody renders the result preview lines for a tool. fromTail reports
// whether truncation should keep the end (command output) or the start.
func (r *renderer) toolBody(b block) ([]string, bool) {
	t := b.tool
	m, _ := t.result.(map[string]any)
	if t.failed {
		msg := formatAny(pick(m, "error", "reason"))
		lines := splitLines(r.st.danger.Render(msg))
		if summary, ok := m["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			lines = append(lines, dimLines(r, summary)...)
		}
		return lines, t.call.Name == "exec"
	}
	switch t.call.Name {
	case "exec", "job":
		if summary, ok := m["summary"].(string); ok {
			return dimLines(r, summary), true
		}
		if id, ok := m["id"].(string); ok {
			return []string{r.st.muted.Render("background job " + id + " · log " + formatAny(m["log"]))}, false
		}
	case "todo":
		return r.todoLines(b.todos), false
	case "edit":
		return r.editLines(t.call.Arguments), false
	case "read":
		return r.readLines(t.result), false
	case "search":
		return r.searchLines(t.result), false
	case "ask", "spawn":
		return nil, false
	}
	return r.genericLines(t.result), false
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func dimLines(r *renderer, s string) []string {
	lines := splitLines(ansi.Strip(s))
	for i, l := range lines {
		lines[i] = r.st.signal.Render(strings.ReplaceAll(l, "\t", "    "))
	}
	return lines
}

func (r *renderer) todoLines(todos []store.Todo) []string {
	out := make([]string, 0, len(todos))
	for _, t := range todos {
		out = append(out, r.todoLine(t))
	}
	return out
}

func (r *renderer) todoLine(t store.Todo) string {
	phase := r.st.muted.Render(fmt.Sprintf("%-9s", t.Phase))
	switch t.Status {
	case "completed":
		return r.st.accentDim.Render("✓ ") + phase + " " + r.st.muted.Render(t.Text)
	case "in_progress":
		return r.st.purple.Render("◆ ") + phase + " " + r.st.bright.Render(t.Text)
	default:
		return r.st.muted.Render("○ ") + phase + " " + r.st.signal.Render(t.Text)
	}
}

func (r *renderer) editLines(args map[string]any) []string {
	hunks, _ := args["hunks"].([]any)
	var out []string
	for _, raw := range hunks {
		h, _ := raw.(map[string]any)
		anchor := formatAny(h["anchor"])
		del, _ := h["delete"].(float64)
		head := "@ " + anchor
		if del > 0 {
			head += fmt.Sprintf("  −%d", int(del))
		}
		out = append(out, r.st.muted.Render(head))
		inserts, _ := h["insert"].([]any)
		for _, line := range inserts {
			out = append(out, r.st.accentDim.Render("+ "+strings.ReplaceAll(formatAny(line), "\t", "    ")))
		}
	}
	return out
}

func (r *renderer) readLines(result any) []string {
	switch x := result.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, raw := range x {
			m, _ := raw.(map[string]any)
			if name, ok := m["name"].(string); ok {
				if dir, _ := m["dir"].(bool); dir {
					name += "/"
				}
				out = append(out, r.st.signal.Render(name))
				continue
			}
			n, _ := m["number"].(float64)
			out = append(out, r.st.muted.Render(fmt.Sprintf("%5d ", int(n)))+r.st.signal.Render(strings.ReplaceAll(formatAny(m["text"]), "\t", "    ")))
		}
		return out
	case map[string]any:
		if count, ok := x["line_count"].(float64); ok {
			return []string{r.st.muted.Render(fmt.Sprintf("%d lines · outline only", int(count)))}
		}
		if text, ok := x["text"].(string); ok {
			return dimLines(r, text)
		}
	}
	return r.genericLines(result)
}

func (r *renderer) searchLines(result any) []string {
	items, ok := result.([]any)
	if !ok {
		return r.genericLines(result)
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		line, _ := m["line"].(float64)
		out = append(out, r.st.accentDim.Render(formatAny(m["path"])+":"+strconv.Itoa(int(line)))+" "+r.st.signal.Render(strings.TrimSpace(formatAny(m["text"]))))
	}
	if len(out) == 0 {
		return []string{r.st.muted.Render("no matches")}
	}
	return out
}

func (r *renderer) genericLines(result any) []string {
	switch x := result.(type) {
	case nil:
		return nil
	case string:
		return dimLines(r, x)
	case map[string]any:
		for _, k := range []string{"summary", "text", "content", "output", "answer", "message"} {
			if s, ok := x[k].(string); ok && strings.TrimSpace(s) != "" {
				return dimLines(r, s)
			}
		}
	}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil
	}
	return dimLines(r, string(b))
}

// toolArgSummary picks the argument a human scans for first.
func toolArgSummary(name string, args map[string]any) string {
	str := func(k string) string { return strings.TrimSpace(formatAny(args[k])) }
	switch name {
	case "exec":
		cmd := str("command")
		if b, _ := args["background"].(bool); b {
			cmd += "  &"
		}
		return cmd
	case "read":
		p := str("path")
		if s := str("start"); s != "" {
			p += ":" + s
			if l := str("limit"); l != "" {
				p += "+" + l
			}
		} else if a := str("around_line"); a != "" {
			p += ":~" + a
		}
		return p
	case "search":
		p := "/" + str("pattern") + "/"
		if g := str("glob"); g != "" {
			p += " " + g
		}
		return p
	case "edit":
		hunks, _ := args["hunks"].([]any)
		ins, del := 0, 0
		for _, raw := range hunks {
			h, _ := raw.(map[string]any)
			lines, _ := h["insert"].([]any)
			ins += len(lines)
			d, _ := h["delete"].(float64)
			del += int(d)
		}
		return fmt.Sprintf("%s  +%d −%d", str("path"), ins, del)
	case "job":
		return str("action") + " " + str("id")
	case "todo":
		items, _ := args["items"].([]any)
		return plural(len(items), "item")
	case "spawn":
		return firstLine(str("spec"))
	case "ask":
		return str("question")
	case "skill":
		return joinNonEmpty(" ", str("action"), str("name"))
	case "web_search":
		return str("query")
	case "fetch", "link":
		return str("url")
	case "lsp":
		return joinNonEmpty(" ", str("operation"), str("path"), str("query"))
	case "artifact":
		return str("path")
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func (r *renderer) worker(b block) string {
	name, spec := "worker", firstLine(b.text)
	if b.review {
		// The engine writes the reviewer spec (instructions plus the diff);
		// the role says more than its first line.
		name, spec = "review", "independent review of the workspace diff"
	}
	if b.result == nil {
		return r.header(r.st.purple, "◇", name, joinNonEmpty(" · ", shortModel(b.model), b.effort, spec), "")
	}
	res := b.result
	glyph, style := "◆", r.st.purple
	if res.Status != agentproto.Pass {
		glyph, style = "◆", r.st.warning
		if res.Status == agentproto.Fail {
			style = r.st.danger
		}
	}
	stats := joinNonEmpty(" · ", string(res.Status), shortModel(b.model), usd(res.Outcome.CostUSD), duration(res.Outcome.Latency), plural(res.Outcome.ToolCalls, "tool"))
	out := r.header(style, glyph, name, stats+"  "+spec, "")
	if res.Error != "" {
		out += "\n" + indent(r.st.danger.Render(r.wrap(res.Error, r.width-6)), "   ", "   ")
	}
	return out
}

var statusMeta = map[agentproto.Status]struct{ glyph, label string }{
	agentproto.Pass:            {"✓", "Passed"},
	agentproto.Fail:            {"✗", "Failed"},
	agentproto.Cancelled:       {"⊘", "Cancelled"},
	agentproto.BudgetExhausted: {"⏱", "Budget exhausted"},
	agentproto.InputRequired:   {"?", "Input required"},
}

func (r *renderer) terminal(b block) string {
	res := b.result
	meta, ok := statusMeta[res.Status]
	if !ok {
		meta = struct{ glyph, label string }{"•", string(res.Status)}
	}
	style := r.st.statusPass
	switch res.Status {
	case agentproto.Pass:
	case agentproto.Fail:
		style = r.st.statusFail
	default:
		style = r.st.warning.Bold(true)
	}
	o := res.Outcome
	stats := []string{duration(o.Latency), usd(o.CostUSD)}
	if o.Tokens > 0 {
		stats = append(stats, tokens(o.Tokens)+" tokens")
	}
	if o.ToolCalls > 0 {
		stats = append(stats, plural(o.ToolCalls, "tool"))
	}
	if o.EditAttempts > 0 {
		stats = append(stats, plural(o.EditAttempts, "edit"))
	}
	line := " " + style.Render(meta.glyph+" "+meta.label) + "  " + r.st.muted.Render(joinNonEmpty(" · ", stats...))
	var flags []string
	if res.Status == agentproto.Pass {
		if o.Verified {
			flags = append(flags, r.st.accentDim.Render("✓ verified"))
		} else {
			flags = append(flags, r.st.muted.Render("unverified"))
		}
	}
	if o.IndependentlyReviewed {
		flags = append(flags, r.st.accentDim.Render("✓ reviewed"))
	}
	if o.ToolErrors > 0 {
		flags = append(flags, r.st.warning.Render(plural(o.ToolErrors, "tool error")))
	}
	if o.EditRetries > 0 {
		flags = append(flags, r.st.warning.Render(plural(o.EditRetries, "edit retry")))
	}
	if o.BudgetReason != "" {
		flags = append(flags, r.st.warning.Render("budget: "+o.BudgetReason))
	}
	if len(flags) > 0 {
		line += "  " + strings.Join(flags, " ")
	}
	out := []string{ansi.Truncate(line, r.width, "…")}
	if res.Error != "" {
		out = append(out, indent(r.st.danger.Render(r.wrap(res.Error, r.width-4)), "   ", "   "))
	}
	if b.structured != nil {
		out = append(out, r.structured(b.structured))
	}
	if res.Status == agentproto.BudgetExhausted {
		out = append(out, r.st.muted.Render("   /budget <usd> raises the ceiling and continues"))
	}
	return strings.Join(out, "\n")
}

// structured renders a JSON completion as readable sections rather than raw
// JSON: status headline, summary prose, string lists as bullets, and the
// remaining scalar fields as key/value lines.
func (r *renderer) structured(v map[string]any) string {
	var out []string
	if s, ok := v["status"].(string); ok && s != "" {
		out = append(out, " "+r.st.accent.Bold(true).Render(s))
	}
	for _, k := range []string{"summary", "answer"} {
		if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, indent(r.markdown(s), " ", " "))
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		switch k {
		case "status", "summary", "answer":
			continue
		}
		keys = append(keys, k)
	}
	order := map[string]int{"changes": 0, "verification": 1, "blockers": 2, "notes": 3}
	sort.SliceStable(keys, func(i, j int) bool {
		oi, iok := order[keys[i]]
		oj, jok := order[keys[j]]
		switch {
		case iok && jok:
			return oi < oj
		case iok != jok:
			return iok
		default:
			return keys[i] < keys[j]
		}
	})
	var scalars []string
	for _, k := range keys {
		switch x := v[k].(type) {
		case []any:
			if len(x) == 0 {
				continue
			}
			out = append(out, " "+r.st.bold.Render(titleCase(k)))
			for _, item := range x {
				out = append(out, indent(r.st.signal.Render(r.wrap(formatAny(item), r.width-6)), "   • ", "     "))
			}
		case map[string]any:
			b, _ := json.Marshal(x)
			scalars = append(scalars, r.st.muted.Render(k+": ")+r.st.signal.Render(ansi.Truncate(string(b), r.width-len(k)-6, "…")))
		default:
			scalars = append(scalars, r.st.muted.Render(k+": ")+r.st.signal.Render(formatAny(x)))
		}
	}
	for _, s := range scalars {
		out = append(out, "   "+ansi.Truncate(s, r.width-4, "…"))
	}
	return strings.Join(out, "\n")
}

func titleCase(k string) string {
	k = strings.ReplaceAll(k, "_", " ")
	if k == "" {
		return k
	}
	return strings.ToUpper(k[:1]) + k[1:]
}

func (r *renderer) inputBlock(in *agentproto.InputRequest) string {
	out := []string{indent(r.st.warning.Bold(true).Render(r.wrap(in.Question, r.width-4)), " "+r.st.warning.Render("?")+" ", "   ")}
	for i, c := range in.Choices {
		out = append(out, r.st.signal.Render(fmt.Sprintf("   %d. %s", i+1, c)))
	}
	return strings.Join(out, "\n")
}

type bannerInfo struct {
	version, backend, sessionID, title, workspace, integration string
}

func (r *renderer) banner(b bannerInfo) string {
	logo := r.st.accent.Render("◉") + " " + r.st.bold.Render("orrery") + r.st.muted.Render(" "+b.version)
	title := b.title
	if title == "" {
		title = "new session"
	}
	lines := []string{" " + logo + r.st.muted.Render("  ·  ") + r.st.bright.Render(ansi.Truncate(title, r.width-24, "…"))}
	meta := joinNonEmpty(" · ", sessionLabel(b.sessionID), b.backend, tildePath(b.workspace), b.integration)
	lines = append(lines, "   "+r.st.muted.Render(ansi.Truncate(meta, r.width-4, "…")))
	keys := []string{"enter send", "shift+enter newline", "esc interrupt", "ctrl+o expand", "ctrl+t thinking", "/ commands"}
	lines = append(lines, "   "+r.st.muted.Render(ansi.Truncate(strings.Join(keys, " · "), r.width-4, "…")))
	return strings.Join(lines, "\n")
}

func sessionLabel(id string) string {
	if id == "" {
		return "session starts with your first message"
	}
	return "session " + shortID(id)
}

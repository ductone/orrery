package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const planWindow = 5

// View draws only the live region: activity, pending work, the composer,
// and the footer. Finished transcript entries are already in scrollback.
func (m *model) View() tea.View {
	if !m.ready || m.quitting {
		return tea.NewView("")
	}
	lines, top := m.layout()
	cursor := m.editor.Cursor()
	if cursor != nil {
		cursor.Y += top
	}
	if pad := m.hold - len(lines); pad > 0 {
		// See guardShrink: keep the previous height for a frame and park
		// the cursor on the first row so the following shrink is exact.
		lines = append(make([]string, pad), lines...)
		cursor = tea.NewCursor(0, 0)
	}
	m.shown, m.shownCursor = len(lines), 0
	if cursor != nil {
		m.shownCursor = cursor.Y
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.Cursor = cursor
	v.WindowTitle = m.windowTitle()
	if m.st.running() {
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, 0)
	}
	return v
}

// layout lays out the live region and reports the editor's first row.
func (m *model) layout() ([]string, int) {
	st := m.r.st
	w := m.width
	var lines []string
	add := func(s ...string) {
		for _, l := range s {
			lines = append(lines, ansi.Truncate(l, w, "…"))
		}
	}
	add("")
	add(m.activityLines()...)
	add(m.pendingLines()...)
	add(m.choiceLines()...)
	labelWidth := 0
	for _, it := range m.items {
		labelWidth = max(labelWidth, ansi.StringWidth(it.label))
	}
	for i, it := range m.items {
		label := it.label + strings.Repeat(" ", labelWidth-ansi.StringWidth(it.label))
		marker, styled := "  ", st.signal.Render(label)
		if i == m.itemIdx {
			marker, styled = st.accent.Render("▸ "), st.bright.Bold(true).Render(label)
		}
		add(" " + marker + styled + "  " + st.muted.Render(it.detail))
	}
	if m.flashText != "" {
		add(" " + m.r.toneStyle(m.flashTone).Render(m.flashText))
	}
	add(m.rule())
	top := len(lines)
	lines = append(lines, strings.Split(m.editor.View(), "\n")...)
	add(m.ruleStyle().Render(strings.Repeat("─", w)))
	add(m.footer())
	return lines, top
}

// guardShrink works around inline-renderer cursor clamping: when the live
// region shrinks below the row the cursor sits on, the renderer redraws from
// the wrong origin and leaves stale rows behind. A shrink of more than the
// rows under the cursor is therefore split in two frames: first the old
// height with the cursor parked on row 0, then the shorter frame.
func (m *model) guardShrink() tea.Cmd {
	if !m.ready || m.quitting {
		return nil
	}
	lines, _ := m.layout()
	h := len(lines)
	if m.hold > 0 {
		if h >= m.hold {
			m.hold = 0
		}
		return nil
	}
	if m.shown > 0 && h < m.shown && m.shownCursor > h-1 {
		m.hold = m.shown
		return tea.Tick(holdFor, func(time.Time) tea.Msg { return releaseMsg{} })
	}
	return nil
}

func (m *model) activityLines() []string {
	st := m.r.st
	now := time.Now()
	var out []string
	switch {
	case m.creating:
		out = append(out, " "+st.purple.Render(spinnerFrames[m.frame%len(spinnerFrames)])+" "+st.bright.Render("Starting session…"))
	case m.st.running():
		label := "Working"
		if m.cancelling {
			label = "Interrupting"
		}
		parts := []string{}
		if m.st.phase != "" {
			parts = append(parts, m.st.phase)
		}
		if m.st.model != "" {
			parts = append(parts, shortModel(m.st.model))
		}
		if !m.st.turnStarted.IsZero() {
			parts = append(parts, clock(now.Sub(m.st.turnStarted)))
		}
		parts = append(parts, "esc to interrupt")
		out = append(out, " "+st.purple.Render(spinnerFrames[m.frame%len(spinnerFrames)])+" "+st.bright.Bold(true).Render(label)+st.muted.Render(" · "+strings.Join(parts, " · ")))
		if t := m.st.tool; t != nil {
			out = append(out, "   "+st.purple.Render("▸ ")+st.toolName.Render(t.call.Name)+"  "+st.toolArg.Render(strings.ReplaceAll(toolArgSummary(t.call.Name, t.call.Arguments), "\n", " ⏎ "))+st.muted.Render("  "+clock(now.Sub(t.started))))
		}
	case m.st.status == "input_required":
		out = append(out, " "+st.warning.Bold(true).Render("?")+" "+st.warning.Render("waiting for your answer"))
	case m.st.status == "interrupted":
		out = append(out, " "+st.muted.Render("◌ interrupted · send a message to continue"))
	case m.st.status == "terminated":
		out = append(out, " "+st.muted.Render("■ session terminated"))
	}
	for _, wk := range m.st.workers {
		name, spec := "worker", firstLine(wk.spec)
		if wk.review {
			name, spec = "review", "independent review of the workspace diff"
		}
		out = append(out, "   "+st.purple.Render("◇ ")+st.toolName.Render(name)+"  "+st.toolArg.Render(joinNonEmpty(" · ", shortModel(wk.model), wk.mode, spec))+st.muted.Render("  "+clock(now.Sub(wk.started))))
	}
	return out
}

func (m *model) pendingLines() []string {
	st := m.r.st
	var out []string
	if m.st.running() && len(m.st.todos) > 0 {
		done, active := 0, -1
		for i, t := range m.st.todos {
			if t.Status == "completed" {
				done++
			}
			if t.Status == "in_progress" && active < 0 {
				active = i
			}
		}
		out = append(out, "   "+st.muted.Render(fmt.Sprintf("☷ plan %d/%d", done, len(m.st.todos))))
		lo := 0
		if active > planWindow/2 {
			lo = active - planWindow/2
		}
		hi := min(len(m.st.todos), lo+planWindow)
		lo = max(0, hi-planWindow)
		for _, t := range m.st.todos[lo:hi] {
			out = append(out, "     "+m.r.todoLine(t))
		}
	}
	queued := m.st.queued
	for i, q := range queued {
		if i == 3 {
			out = append(out, "   "+st.muted.Render(fmt.Sprintf("  +%d more queued", len(queued)-3)))
			break
		}
		out = append(out, "   "+st.accentDim.Render("↳ queued ")+st.signal.Render(firstLine(q.content)))
	}
	if o := m.inflight; o != nil && !m.creating {
		out = append(out, "   "+st.accentDim.Render("↳ sending ")+st.signal.Render(firstLine(clean(o.text))))
	}
	for _, o := range m.outbox {
		label := "↳ pending "
		if m.stalled {
			label = "↳ not sent "
		}
		out = append(out, "   "+st.accentDim.Render(label)+st.signal.Render(firstLine(clean(o.text))))
	}
	return out
}

func (m *model) choiceLines() []string {
	in := m.st.input
	if in == nil || len(in.Choices) == 0 {
		return nil
	}
	st := m.r.st
	out := []string{" " + st.warning.Render("? ") + st.bright.Render(firstLine(in.Question))}
	for i, c := range in.Choices {
		if i == m.choice {
			out = append(out, "   "+st.accent.Render("▸ ")+st.bright.Bold(true).Render(c))
		} else {
			out = append(out, "     "+st.signal.Render(c))
		}
	}
	hint := "↑↓ navigate · enter to select"
	if in.AllowFreeform {
		hint += " · or type an answer"
	}
	return append(out, "   "+st.muted.Render(hint))
}

func (m *model) ruleStyle() lipgloss.Style {
	switch {
	case m.st.input != nil:
		return lipgloss.NewStyle().Foreground(colorWarning)
	case m.st.running() || m.creating:
		return lipgloss.NewStyle().Foreground(colorConductor)
	default:
		return lipgloss.NewStyle().Foreground(colorCircuit)
	}
}

// rule is the composer's top border, labelled with the session's state.
func (m *model) rule() string {
	style := m.ruleStyle()
	label := ""
	switch {
	case m.sessionID == "" && !m.creating:
		label = "new session"
	case m.st.input != nil:
		label = "input required"
	case len(m.st.queued)+len(m.outbox) > 0:
		label = fmt.Sprintf("%d queued", len(m.st.queued)+len(m.outbox))
	}
	if label == "" {
		return style.Render(strings.Repeat("─", m.width))
	}
	head := "── " + label + " "
	return style.Render(head + strings.Repeat("─", max(0, m.width-ansi.StringWidth(head))))
}

func (m *model) footer() string {
	st := m.r.st
	sep := st.muted.Render(" · ")
	var left []string
	if ws := tildePath(m.workspace()); ws != "" {
		left = append(left, st.signal.Render(ws))
	}
	if m.sessionID != "" {
		left = append(left, st.muted.Render(shortID(m.sessionID)))
	}
	if m.st.model != "" {
		mdl := st.accent.Render(shortModel(m.st.model))
		if m.st.effort != "" {
			mdl += st.muted.Render(" " + m.st.effort)
		}
		left = append(left, mdl)
	}
	var right []string
	if m.st.tokensIn+m.st.tokensOut > 0 {
		usage := "↑" + tokens(m.st.tokensIn) + " ↓" + tokens(m.st.tokensOut)
		if m.st.cacheRead > 0 {
			usage += " ⟲" + tokens(m.st.cacheRead)
		}
		right = append(right, st.muted.Render(usage))
	}
	spend := usd(m.st.spend())
	if b := m.sessionSnapshot.BudgetUSD; b > 0 {
		spend += "/" + usd(b)
	}
	spendStyle := st.signal
	if b := m.sessionSnapshot.BudgetUSD; b > 0 && m.st.spend() >= b*0.8 {
		spendStyle = st.warning
	}
	right = append(right, spendStyle.Render(spend))
	if pct, ok := contextPercent(m.st.model, m.st.contextTokens); ok {
		style, label := st.muted, fmt.Sprintf("ctx %d%%", pct)
		if pct == 0 {
			label = "ctx <1%"
		}
		if pct >= 60 {
			style = st.warning
		}
		right = append(right, style.Render(label))
	}
	badge := "local"
	if _, local := m.backend.(*Local); !local {
		badge = "remote"
	}
	if m.squire != nil {
		badge += " · squire"
	}
	right = append(right, st.muted.Render(badge))

	l := " " + strings.Join(left, sep)
	r := strings.Join(right, sep) + " "
	gap := m.width - ansi.StringWidth(l) - ansi.StringWidth(r)
	if gap < 2 {
		l = ansi.Truncate(l, max(0, m.width-ansi.StringWidth(r)-2), "…")
		gap = m.width - ansi.StringWidth(l) - ansi.StringWidth(r)
	}
	if gap < 1 {
		return ansi.Truncate(r, m.width, "…")
	}
	return l + strings.Repeat(" ", gap) + r
}

func (m *model) windowTitle() string {
	title := m.title()
	if title == "" {
		title = "new session"
	}
	state := "idle"
	switch {
	case m.st.running() || m.creating:
		state = "working"
	case m.st.input != nil:
		state = "input required"
	case m.st.status != "":
		state = strings.ReplaceAll(m.st.status, "_", " ")
	}
	return "orrery · " + title + " · " + state
}

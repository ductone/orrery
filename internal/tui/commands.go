package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	catalog "github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
)

type command struct {
	name, args, help string
	needsSession     bool
	run              func(m *model, arg string) tea.Cmd
}

func commandList() []command {
	return []command{
		{name: "help", help: "keys and commands", run: (*model).cmdHelp},
		{name: "status", help: "session, budget, and integration details", needsSession: true, run: (*model).cmdStatus},
		{name: "cancel", help: "interrupt the running turn (esc)", needsSession: true, run: (*model).cmdCancel},
		{name: "budget", args: "<usd>", help: "raise the spend ceiling; resumes a budget-stopped turn", needsSession: true, run: (*model).cmdBudget},
		{name: "compact", help: "compact history into durable state", needsSession: true, run: (*model).cmdCompact},
		{name: "checkpoint", args: "[label]", help: "snapshot conversational state", needsSession: true, run: (*model).cmdCheckpoint},
		{name: "checkpoints", help: "list checkpoints", needsSession: true, run: (*model).cmdCheckpoints},
		{name: "restore", args: "<#|id>", help: "restore a checkpoint; workspace files are never touched", needsSession: true, run: (*model).cmdRestore},
		{name: "copy", help: "copy the last answer to the clipboard", run: (*model).cmdCopy},
		{name: "expand", help: "toggle full tool output (ctrl+o)", run: func(m *model, _ string) tea.Cmd { return m.toggleExpand() }},
		{name: "thinking", help: "toggle model reasoning (ctrl+t)", run: func(m *model, _ string) tea.Cmd { return m.toggleThinking() }},
		{name: "redraw", help: "re-render the transcript (ctrl+l)", run: func(m *model, _ string) tea.Cmd { return m.rerender() }},
		{name: "quit", help: "exit; the session stays resumable", run: func(m *model, _ string) tea.Cmd { return m.quit() }},
	}
}

func findCommand(name string) (command, bool) {
	for _, c := range commandList() {
		if c.name == name || (name == "exit" && c.name == "quit") {
			return c, true
		}
	}
	return command{}, false
}

// parseCommand recognizes "/name args". A leading slash followed by a path
// ("/tmp/x is broken") is ordinary message text.
func parseCommand(text string) (name, arg string, ok bool) {
	if !strings.HasPrefix(text, "/") || strings.HasPrefix(text, "//") {
		return "", "", false
	}
	head, rest, _ := strings.Cut(text[1:], " ")
	if head == "" || strings.ContainsAny(head, "/.\n\t") {
		return "", "", false
	}
	return head, strings.TrimSpace(rest), true
}

// actionMsg reports the outcome of an asynchronous command.
type actionMsg struct {
	flash string
	tone  tone
	err   error
	panel *block
}

func (m *model) action(timeout time.Duration, fn func(ctx context.Context) actionMsg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, timeout)
		defer cancel()
		return fn(ctx)
	}
}

func panel(fn func(r *renderer) string) *block {
	return &block{kind: blockPanel, panel: fn}
}

func (m *model) cmdHelp(string) tea.Cmd {
	keys := [][2]string{
		{"enter", "send · queues while a turn runs"},
		{"shift+enter", "newline (also alt+enter, ctrl+j, or a trailing \\)"},
		{"esc", "interrupt the running turn"},
		{"ctrl+c", "clear input · twice to exit"},
		{"ctrl+d", "exit when input is empty"},
		{"↑ ↓", "prompt history · choose an answer"},
		{"tab", "complete /commands and @files"},
		{"ctrl+o", "expand tool output"},
		{"ctrl+t", "show model reasoning"},
		{"ctrl+l", "redraw"},
	}
	cmds := commandList()
	return m.commit(*panel(func(r *renderer) string {
		var out []string
		out = append(out, " "+r.st.bold.Render("Keys"))
		for _, k := range keys {
			out = append(out, "   "+r.st.key.Render(fmt.Sprintf("%-12s", k[0]))+" "+r.st.signal.Render(k[1]))
		}
		out = append(out, " "+r.st.bold.Render("Commands"))
		for _, c := range cmds {
			name := "/" + c.name
			if c.args != "" {
				name += " " + c.args
			}
			out = append(out, "   "+r.st.accent.Render(fmt.Sprintf("%-20s", name))+" "+r.st.signal.Render(c.help))
		}
		out = append(out, "   "+r.st.muted.Render("@path mentions a workspace file · //text sends a message starting with /"))
		for i := range out {
			out[i] = ansi.Truncate(out[i], r.width, "…")
		}
		return strings.Join(out, "\n")
	}))
}

func (m *model) cmdStatus(string) tea.Cmd {
	id := m.sessionID
	return m.action(15*time.Second, func(ctx context.Context) actionMsg {
		s, err := m.backend.Session(ctx, id)
		if err != nil {
			return actionMsg{err: err}
		}
		rows := [][2]string{
			{"session", s.ID},
			{"title", s.DisplayTitle()},
			{"status", joinNonEmpty(" · ", s.Status, "phase "+s.Phase, fmt.Sprintf("turn %d", s.Turn))},
			{"model", s.Model},
			{"spend", usd(s.SpentUSD) + " of " + usd(s.BudgetUSD)},
			{"workspace", s.WorkspacePath},
			{"backend", m.backend.Describe()},
		}
		if s.Integration != "" {
			rows = append(rows, [2]string{"integration", joinNonEmpty(" · ", s.Integration, s.ExternalID, s.ExternalIncarnation)})
		}
		if m.squire != nil {
			rows = append(rows, [2]string{"control", m.squire.SocketPath()})
		}
		return actionMsg{panel: panel(func(r *renderer) string {
			out := []string{" " + r.st.bold.Render("Session")}
			for _, row := range rows {
				if row[1] == "" {
					continue
				}
				out = append(out, ansi.Truncate("   "+r.st.muted.Render(fmt.Sprintf("%-12s", row[0]))+" "+r.st.bright.Render(clean(row[1])), r.width, "…"))
			}
			return strings.Join(out, "\n")
		})}
	})
}

func (m *model) cmdCancel(string) tea.Cmd {
	if !m.st.running() {
		return m.flashCmd("nothing is running", toneDim)
	}
	m.cancelling = true
	id := m.sessionID
	return m.action(15*time.Second, func(ctx context.Context) actionMsg {
		ok, err := m.backend.Cancel(ctx, id)
		if err != nil {
			return actionMsg{err: err}
		}
		if !ok {
			return actionMsg{flash: "no active turn to interrupt", tone: toneDim}
		}
		return actionMsg{flash: "interrupting…", tone: toneWarn}
	})
}

func (m *model) cmdBudget(arg string) tea.Cmd {
	amount, err := strconv.ParseFloat(strings.TrimPrefix(arg, "$"), 64)
	if err != nil || !(amount > 0) {
		return m.flashCmd("usage: /budget <usd>, e.g. /budget 5", toneWarn)
	}
	id := m.sessionID
	return m.action(15*time.Second, func(ctx context.Context) actionMsg {
		resumed, err := m.backend.AddBudget(ctx, id, amount)
		if err != nil {
			return actionMsg{err: err}
		}
		if resumed {
			return actionMsg{flash: "budget raised by " + usd(amount) + " · resuming", tone: toneGood}
		}
		return actionMsg{flash: "budget raised by " + usd(amount), tone: toneGood}
	})
}

func (m *model) cmdCompact(string) tea.Cmd {
	if m.st.running() {
		return m.flashCmd("compaction runs between turns; wait or press esc", toneWarn)
	}
	id := m.sessionID
	m.flash("compacting…", toneDim)
	return m.action(5*time.Minute, func(ctx context.Context) actionMsg {
		if err := m.backend.Compact(ctx, id); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{flash: "context compacted", tone: toneGood}
	})
}

func (m *model) cmdCheckpoint(arg string) tea.Cmd {
	label := arg
	if label == "" {
		label = "Manual checkpoint"
	}
	id := m.sessionID
	return m.action(30*time.Second, func(ctx context.Context) actionMsg {
		cp, err := m.backend.Checkpoint(ctx, id, label)
		if err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{flash: "checkpoint " + shortID(cp.ID) + " · " + cp.Label, tone: toneGood}
	})
}

func (m *model) cmdCheckpoints(string) tea.Cmd {
	id := m.sessionID
	return m.action(15*time.Second, func(ctx context.Context) actionMsg {
		cps, err := m.backend.Checkpoints(ctx, id)
		if err != nil {
			return actionMsg{err: err}
		}
		if len(cps) == 0 {
			return actionMsg{flash: "no checkpoints yet · /checkpoint [label]", tone: toneDim}
		}
		return actionMsg{panel: panel(func(r *renderer) string {
			out := []string{" " + r.st.bold.Render("Checkpoints") + r.st.muted.Render("  /restore <#|id>")}
			for i, cp := range cps {
				line := fmt.Sprintf("   %s %s  %s %s", r.st.accent.Render(fmt.Sprintf("#%-2d", i+1)), r.st.muted.Render(shortID(cp.ID)), r.st.bright.Render(clean(cp.Label)), r.st.muted.Render("· "+joinNonEmpty(" · ", clean(cp.Reason), cp.CreatedAt.Local().Format("Jan 2 15:04"))))
				out = append(out, ansi.Truncate(line, r.width, "…"))
			}
			return strings.Join(out, "\n")
		})}
	})
}

func (m *model) cmdRestore(arg string) tea.Cmd {
	if arg == "" {
		return m.flashCmd("usage: /restore <#|id> · list with /checkpoints", toneWarn)
	}
	if m.st.running() {
		return m.flashCmd("restore runs between turns; wait or press esc", toneWarn)
	}
	id := m.sessionID
	return m.action(30*time.Second, func(ctx context.Context) actionMsg {
		cps, err := m.backend.Checkpoints(ctx, id)
		if err != nil {
			return actionMsg{err: err}
		}
		target, err := selectCheckpoint(cps, arg)
		if err != nil {
			return actionMsg{err: err}
		}
		if err := m.backend.Restore(ctx, id, target.ID); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{flash: "restored " + shortID(target.ID) + " · " + target.Label, tone: toneGood}
	})
}

func selectCheckpoint(cps []store.Checkpoint, arg string) (store.Checkpoint, error) {
	arg = strings.TrimPrefix(arg, "#")
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(cps) && len(arg) < 4 {
		return cps[n-1], nil
	}
	var match []store.Checkpoint
	for _, cp := range cps {
		if strings.HasPrefix(cp.ID, arg) {
			match = append(match, cp)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return store.Checkpoint{}, fmt.Errorf("no checkpoint matches %q", arg)
	default:
		return store.Checkpoint{}, fmt.Errorf("%q matches %d checkpoints", arg, len(match))
	}
}

func (m *model) cmdCopy(string) tea.Cmd {
	if m.st.lastAnswer == "" {
		return m.flashCmd("no answer to copy yet", toneDim)
	}
	m.flash(fmt.Sprintf("copied %s chars", tokens(len(m.st.lastAnswer))), toneGood)
	return tea.SetClipboard(m.st.lastAnswer)
}

// contextPercent is the last root prompt as a share of the model's window.
func contextPercent(modelID string, used int) (int, bool) {
	spec, ok := catalog.Get(modelID)
	if !ok || spec.ContextWindow == 0 || used == 0 {
		return 0, false
	}
	return used * 100 / spec.ContextWindow, true
}

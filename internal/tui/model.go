package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"

	"github.com/ductone/orrey/internal/store"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	frameInterval   = 90 * time.Millisecond
	sessionInterval = 2 * time.Second
	doublePress     = 1500 * time.Millisecond
	flashFor        = 4 * time.Second
	maxPopup        = 8
	// repaintSettle spans a few renderer frames (60 fps by default).
	repaintSettle = 60 * time.Millisecond
	// holdFor keeps a shrinking live region at its old height for a few
	// renderer frames; see guardShrink.
	holdFor = 50 * time.Millisecond
	// clearScrollback is ED 3, which also discards erased screens that
	// terminals such as tmux move into history.
	clearScrollback = ansi.EraseEntireDisplay
)

type (
	eventsMsg   []Event
	frameMsg    struct{}
	sessionTick struct{}
	sessionMsg  struct {
		s         store.Session
		err       error
		recurring bool
	}
	resizeMsg   struct{ gen int }
	printedMsg  struct{}
	releaseMsg  struct{}
	flashExpiry struct{ gen int }
	filesMsg    []string
	// deliveredMsg reports the engine's answer to a submitted message.
	deliveredMsg struct {
		created string
		result  SendResult
		err     error
	}
	// controlMsg is a prompt that arrived over the Squire control socket.
	controlMsg struct {
		text  string
		reply chan<- error
	}
)

// outgoing is a message waiting for delivery. reply is set for control
// socket prompts, which learn the outcome; typed text is restored instead.
type outgoing struct {
	text  string
	reply chan<- error
}

type completionItem struct{ label, detail, insert string }

type model struct {
	ctx       context.Context
	opts      Options
	backend   Backend
	squire    *Squire
	sessionID string

	st              state
	sessionSnapshot store.Session
	history         []block
	prev            block
	printQueue      []string
	printing        bool
	redraw          bool
	shown           int
	shownCursor     int
	hold            int
	profile         colorprofile.Profile
	r               *renderer
	ready           bool
	width, height   int
	resizeGen       int

	editor     textarea.Model
	events     chan []Event
	frame      int
	ticking    bool
	creating   bool
	outbox     []outgoing
	inflight   *outgoing
	stalled    bool
	polling    bool
	cancelling bool

	flashText string
	flashTone tone
	flashGen  int

	ctrlCAt  time.Time
	quitting bool

	prompts  []string
	histIdx  int
	browsing bool
	draft    string

	items     []completionItem
	itemIdx   int
	dismissed string
	files     []string
	indexing  bool

	choice int
}

func newModel(ctx context.Context, opts Options, sessionID string) *model {
	m := &model{ctx: ctx, opts: opts, backend: opts.Backend, sessionID: sessionID, r: newRenderer()}
	ed := textarea.New()
	ed.ShowLineNumbers = false
	ed.DynamicHeight = true
	ed.MinHeight = 1
	ed.MaxHeight = 10
	ed.MaxContentHeight = 1 << 20
	ed.CharLimit = 0
	ed.SetVirtualCursor(false)
	ed.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	ed.KeyMap.TransposeCharacterBackward = key.NewBinding(key.WithDisabled())
	s := ed.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Focused.Placeholder = lipgloss.NewStyle().Foreground(colorMuted)
	s.Focused.Text = lipgloss.NewStyle().Foreground(colorBright)
	s.Cursor.Color = colorElectric
	s.Cursor.Shape = tea.CursorBar
	s.Cursor.Blink = false
	ed.SetStyles(s)
	accent := lipgloss.NewStyle().Foreground(colorElectric).Bold(true)
	ed.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return accent.Render("❯") + " "
		}
		return "  "
	})
	m.editor = ed
	m.updatePlaceholder()
	return m
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.editor.Focus()}
	if m.sessionID != "" {
		cmds = append(cmds, m.startStream(), m.startPolling())
	}
	if strings.TrimSpace(m.opts.Prompt) != "" {
		cmds = append(cmds, m.deliver(outgoing{text: m.opts.Prompt}))
	}
	return tea.Batch(cmds...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	return m, tea.Batch(cmd, m.guardShrink())
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case releaseMsg:
		m.hold = 0
	case tea.ColorProfileMsg:
		m.profile = msg.Profile
	case tea.WindowSizeMsg:
		return m.resize(msg.Width, msg.Height)
	case resizeMsg:
		if msg.gen == m.resizeGen {
			return m.rerender()
		}
	case printedMsg:
		m.printing = false
		if m.redraw {
			m.redraw = false
			return m.repaint()
		}
		return m.flushPrint()
	case eventsMsg:
		return m.onEvents(msg)
	case frameMsg:
		m.frame++
		if m.animating() {
			return tea.Tick(frameInterval, func(time.Time) tea.Msg { return frameMsg{} })
		}
		m.ticking = false
	case sessionTick:
		return m.fetchSession(true)
	case sessionMsg:
		if msg.err == nil {
			m.sessionSnapshot = msg.s
			m.st.reconcile(msg.s)
			if !m.st.running() {
				m.cancelling = false
			}
			m.updatePlaceholder()
		}
		if !msg.recurring {
			return m.tick()
		}
		return tea.Batch(m.tick(), tea.Tick(sessionInterval, func(time.Time) tea.Msg { return sessionTick{} }))
	case deliveredMsg:
		return m.onDelivered(msg)
	case controlMsg:
		return m.deliver(outgoing{text: msg.text, reply: msg.reply})
	case actionMsg:
		var cmds []tea.Cmd
		if msg.err != nil {
			m.flash(msg.err.Error(), toneError)
		} else if msg.flash != "" {
			m.flash(msg.flash, msg.tone)
		}
		if msg.panel != nil {
			cmds = append(cmds, m.commit(*msg.panel))
		}
		cmds = append(cmds, m.flashTimer(), m.fetchSession(false))
		return tea.Batch(cmds...)
	case flashExpiry:
		if msg.gen == m.flashGen {
			m.flashText = ""
		}
	case filesMsg:
		m.files, m.indexing = msg, false
		m.refreshCompletion()
	case tea.PasteMsg:
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		return tea.Batch(cmd, m.afterEdit())
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return nil
}

func (m *model) resize(w, h int) tea.Cmd {
	widthChanged := w != m.width
	m.width, m.height = w, h
	m.r.setWidth(w)
	m.editor.SetWidth(w - 1)
	if !m.ready {
		m.ready = true
		return m.print(m.renderAll())
	}
	if !widthChanged {
		return nil
	}
	m.resizeGen++
	gen := m.resizeGen
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return resizeMsg{gen} })
}

func (m *model) startStream() tea.Cmd {
	m.events = make(chan []Event, 32)
	ch, id := m.events, m.sessionID
	go func() { _ = m.backend.Stream(m.ctx, id, 0, ch) }()
	return m.waitEvents()
}

func (m *model) waitEvents() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		select {
		case evs := <-ch:
			for {
				select {
				case more := <-ch:
					evs = append(evs, more...)
				default:
					return eventsMsg(evs)
				}
			}
		case <-m.ctx.Done():
			return nil
		}
	}
}

func (m *model) onEvents(evs []Event) tea.Cmd {
	if m.squire != nil {
		m.squire.Record(evs)
	}
	wasRunning := m.st.running()
	var blocks []block
	for _, ev := range evs {
		blocks = append(blocks, m.st.apply(ev)...)
	}
	if wasRunning && !m.st.running() {
		m.cancelling = false
	}
	if m.st.input == nil {
		m.choice = 0
	}
	m.updatePlaceholder()
	return tea.Batch(m.commit(blocks...), m.waitEvents(), m.tick())
}

// startPolling begins the single recurring snapshot refresh; one-shot
// refreshes after commands use fetchSession(false) and never re-arm it.
func (m *model) startPolling() tea.Cmd {
	if m.polling {
		return nil
	}
	m.polling = true
	return m.fetchSession(true)
}

func (m *model) fetchSession(recurring bool) tea.Cmd {
	if m.sessionID == "" {
		return nil
	}
	id := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		s, err := m.backend.Session(ctx, id)
		return sessionMsg{s: s, err: err, recurring: recurring}
	}
}

func (m *model) animating() bool {
	return m.st.running() || len(m.st.workers) > 0 || m.creating
}

func (m *model) tick() tea.Cmd {
	if m.ticking || !m.animating() {
		return nil
	}
	m.ticking = true
	return tea.Tick(frameInterval, func(time.Time) tea.Msg { return frameMsg{} })
}

// commit records blocks in the transcript and prints them into scrollback.
func (m *model) commit(blocks ...block) tea.Cmd {
	if len(blocks) == 0 {
		return nil
	}
	m.history = append(m.history, blocks...)
	if !m.ready {
		return nil
	}
	var b strings.Builder
	for _, blk := range blocks {
		m.appendRendered(&b, blk)
	}
	return m.print(b.String())
}

func (m *model) appendRendered(b *strings.Builder, blk block) {
	text := m.r.render(blk)
	if text == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	if !compact(m.prev) || !compact(blk) {
		b.WriteByte('\n')
	}
	b.WriteString(text)
	m.prev = blk
}

// print queues text for scrollback. Bubble Tea inserts printed lines above
// the live region by scrolling the screen, which is only exact when a chunk
// fits between the top of the screen and the live region, and concurrent
// print commands may reorder. So text is split into screen-sized chunks and
// printed strictly one at a time: each chunk's printedMsg releases the next.
func (m *model) print(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	m.printQueue = append(m.printQueue, m.chunks(text)...)
	return m.flushPrint()
}

func (m *model) chunks(text string) []string {
	room := max(1, m.height-max(m.shown, m.hold)-1)
	lines := strings.Split(text, "\n")
	var out []string
	for len(lines) > room {
		out = append(out, strings.Join(lines[:room], "\n"))
		lines = lines[room:]
	}
	return append(out, strings.Join(lines, "\n"))
}

// downsample converts truecolor styling for terminals with fewer colors.
// The renderer does this for the live region, but printed lines bypass it.
func (m *model) downsample(text string) string {
	if m.profile == colorprofile.Unknown || m.profile == colorprofile.TrueColor {
		return text
	}
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: m.profile}
	if _, err := w.WriteString(text); err != nil {
		return text
	}
	return b.String()
}

func (m *model) flushPrint() tea.Cmd {
	if m.printing || len(m.printQueue) == 0 {
		return nil
	}
	m.printing = true
	next := m.downsample(m.printQueue[0])
	m.printQueue = m.printQueue[1:]
	return tea.Sequence(tea.Println(next), func() tea.Msg { return printedMsg{} })
}

func (m *model) renderAll() string {
	var b strings.Builder
	b.WriteString(m.r.banner(bannerInfo{
		version:     m.opts.Version,
		backend:     m.backend.Describe(),
		sessionID:   m.sessionID,
		title:       m.title(),
		workspace:   m.workspace(),
		integration: m.integrationLabel(),
	}))
	m.prev = block{kind: blockPanel}
	for _, blk := range m.history {
		m.appendRendered(&b, blk)
	}
	return b.String()
}

// rerender repaints the whole transcript, which is how width changes and
// the expand/thinking toggles reach blocks already in scrollback.
func (m *model) rerender() tea.Cmd {
	if !m.ready {
		return nil
	}
	m.printQueue = nil
	if m.printing {
		m.redraw = true
		return nil
	}
	return m.repaint()
}

// repaint clears the screen, lets the renderer flush the cleared frame, and
// only then reprints the transcript above it: a clear still pending at flush
// time would erase freshly inserted lines. Scrollback is cleared inside the
// first printed chunk because raw output is flushed with frames, while
// printed lines are written immediately, so a separate clear could land
// after the reprint.
func (m *model) repaint() tea.Cmd {
	m.printQueue = m.chunks(m.renderAll())
	m.printQueue[0] = clearScrollback + m.printQueue[0]
	m.printing = true
	return tea.Sequence(
		tea.Raw(ansi.CursorHomePosition+ansi.EraseEntireScreen+clearScrollback),
		tea.ClearScreen,
		tea.Tick(repaintSettle, func(time.Time) tea.Msg { return printedMsg{} }),
	)
}

func (m *model) toggleExpand() tea.Cmd {
	m.r.expand = !m.r.expand
	m.flash(map[bool]string{true: "tool output expanded", false: "tool output collapsed"}[m.r.expand], toneDim)
	return tea.Batch(m.rerender(), m.flashTimer())
}

func (m *model) toggleThinking() tea.Cmd {
	m.r.thinking = !m.r.thinking
	m.flash(map[bool]string{true: "showing reasoning", false: "reasoning hidden"}[m.r.thinking], toneDim)
	return tea.Batch(m.rerender(), m.flashTimer())
}

func (m *model) quit() tea.Cmd {
	m.quitting = true
	return tea.Quit
}

func (m *model) flash(text string, t tone) {
	m.flashText, m.flashTone = firstLine(clean(text)), t
	m.flashGen++
}

func (m *model) flashTimer() tea.Cmd {
	gen := m.flashGen
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashExpiry{gen} })
}

func (m *model) flashCmd(text string, t tone) tea.Cmd {
	m.flash(text, t)
	return m.flashTimer()
}

func (m *model) onKey(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k != "ctrl+c" {
		m.ctrlCAt = time.Time{}
	}
	if len(m.items) > 0 {
		switch k {
		case "up", "ctrl+p":
			m.itemIdx = (m.itemIdx + len(m.items) - 1) % len(m.items)
			return nil
		case "down", "ctrl+n":
			m.itemIdx = (m.itemIdx + 1) % len(m.items)
			return nil
		case "tab":
			m.acceptCompletion()
			return nil
		case "esc":
			m.dismissed = m.editor.Value()
			m.items = nil
			return nil
		case "enter":
			item := m.items[m.itemIdx]
			if strings.TrimSpace(item.insert) != strings.TrimSpace(m.editor.Value()) {
				m.acceptCompletion()
				return nil
			}
		}
	}
	if m.st.input != nil && len(m.st.input.Choices) > 0 && m.editor.Value() == "" {
		n := len(m.st.input.Choices)
		switch k {
		case "up":
			m.choice = (m.choice + n - 1) % n
			return nil
		case "down":
			m.choice = (m.choice + 1) % n
			return nil
		case "enter":
			return m.submit(m.st.input.Choices[m.choice])
		}
		if !m.st.input.AllowFreeform && len(k) == 1 && k[0] >= '1' && int(k[0]-'0') <= n {
			return m.submit(m.st.input.Choices[k[0]-'1'])
		}
	}
	switch k {
	case "enter":
		value := m.editor.Value()
		if strings.HasSuffix(value, "\\") {
			m.editor.SetValue(strings.TrimSuffix(value, "\\") + "\n")
			m.editor.MoveToEnd()
			m.afterEdit()
			return nil
		}
		if strings.TrimSpace(value) == "" && m.stalled {
			m.stalled = false
			return m.pump()
		}
		return m.submit(value)
	case "esc":
		if m.st.running() && m.sessionID != "" {
			return m.cmdCancel("")
		}
		return nil
	case "ctrl+c":
		if m.editor.Value() != "" {
			m.editor.Reset()
			m.afterEdit()
			return nil
		}
		if !m.ctrlCAt.IsZero() && time.Since(m.ctrlCAt) < doublePress {
			return m.quit()
		}
		m.ctrlCAt = time.Now()
		hint := "press ctrl+c again to exit"
		if m.st.running() {
			if _, local := m.backend.(*Local); local {
				hint += " · the running turn will stop"
			} else {
				hint += " · the turn keeps running on the server"
			}
		}
		return m.flashCmd(hint, toneDim)
	case "ctrl+d":
		if m.editor.Value() == "" {
			return m.quit()
		}
	case "ctrl+o":
		return m.toggleExpand()
	case "ctrl+t":
		return m.toggleThinking()
	case "ctrl+l":
		return m.rerender()
	case "up":
		if m.editor.Line() == 0 && (m.editor.Value() == "" || m.browsing) && len(m.prompts) > 0 {
			if !m.browsing {
				m.draft, m.browsing, m.histIdx = m.editor.Value(), true, len(m.prompts)
			}
			if m.histIdx > 0 {
				m.histIdx--
				m.editor.SetValue(m.prompts[m.histIdx])
			}
			return nil
		}
	case "down":
		if m.browsing && m.editor.Line() == m.editor.LineCount()-1 {
			m.histIdx++
			if m.histIdx >= len(m.prompts) {
				m.browsing = false
				m.editor.SetValue(m.draft)
			} else {
				m.editor.SetValue(m.prompts[m.histIdx])
			}
			return nil
		}
	case "tab":
		m.dismissed = ""
		m.refreshCompletion()
		if len(m.items) == 1 {
			m.acceptCompletion()
		}
		return nil
	}
	m.browsing = false
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	return tea.Batch(cmd, m.afterEdit())
}

func (m *model) afterEdit() tea.Cmd {
	m.refreshCompletion()
	if _, ok := mentionToken(m.editor.Value()); ok && m.files == nil && !m.indexing {
		m.indexing = true
		root := m.workspace()
		return func() tea.Msg { return filesMsg(indexFiles(root)) }
	}
	return nil
}

func (m *model) refreshCompletion() {
	value := m.editor.Value()
	prevLabel := ""
	if len(m.items) > 0 {
		prevLabel = m.items[m.itemIdx].label
	}
	m.items = nil
	if value == "" || value == m.dismissed {
		return
	}
	if strings.HasPrefix(value, "/") && !strings.ContainsAny(value, " \n") {
		for _, c := range commandList() {
			if strings.HasPrefix("/"+c.name, value) {
				insert := "/" + c.name
				if c.args != "" {
					insert += " "
				}
				m.items = append(m.items, completionItem{label: strings.TrimSpace("/" + c.name + " " + c.args), detail: c.help, insert: insert})
			}
		}
	} else if token, ok := mentionToken(value); ok && m.files != nil {
		for _, f := range fuzzyFilter(token, m.files, maxPopup) {
			m.items = append(m.items, completionItem{label: "@" + f, insert: value[:len(value)-len(token)-1] + "@" + f + " "})
		}
	}
	m.itemIdx = 0
	for i, it := range m.items {
		if it.label == prevLabel {
			m.itemIdx = i
		}
	}
}

func (m *model) acceptCompletion() {
	if len(m.items) == 0 {
		return
	}
	m.editor.SetValue(m.items[m.itemIdx].insert)
	m.editor.MoveToEnd()
	m.items = nil
	m.refreshCompletion()
}

// submit handles text typed into the composer: slash commands run locally,
// everything else is delivered to the session.
func (m *model) submit(value string) tea.Cmd {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	m.items = nil
	if name, arg, ok := parseCommand(text); ok {
		c, found := findCommand(name)
		if !found {
			return m.flashCmd(fmt.Sprintf("unknown command /%s · /help lists commands · //%s sends it as text", name, name), toneWarn)
		}
		m.editor.Reset()
		if c.needsSession && m.sessionID == "" {
			return m.flashCmd("no session yet · send a message to start one", toneDim)
		}
		return c.run(m, arg)
	}
	if strings.HasPrefix(text, "//") {
		text = text[1:]
	}
	if in := m.st.input; in != nil && !in.AllowFreeform && len(in.Choices) > 0 && !contains(in.Choices, text) {
		return m.flashCmd("choose one of the offered answers (↑↓ enter)", toneWarn)
	}
	m.editor.Reset()
	m.afterEdit()
	if len(m.prompts) == 0 || m.prompts[len(m.prompts)-1] != text {
		m.prompts = append(m.prompts, text)
	}
	m.browsing = false
	return m.deliver(outgoing{text: text})
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// deliver queues a message for the session. Deliveries run strictly one at
// a time in submission order, because Bubble Tea runs commands concurrently
// and the engine orders messages by arrival. The first delivery creates the
// session when the TUI started without one.
func (m *model) deliver(o outgoing) tea.Cmd {
	m.outbox = append(m.outbox, o)
	m.stalled = false
	return m.pump()
}

func (m *model) pump() tea.Cmd {
	if m.inflight != nil || m.stalled || len(m.outbox) == 0 {
		return nil
	}
	o := m.outbox[0]
	m.outbox = m.outbox[1:]
	m.inflight = &o
	if m.sessionID == "" {
		m.creating = true
		req := m.opts.Create
		req.Prompt = o.text
		return tea.Batch(m.tick(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
			defer cancel()
			id, err := m.backend.Create(ctx, req)
			return deliveredMsg{created: id, err: err}
		})
	}
	id := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		defer cancel()
		res, err := m.backend.Send(ctx, id, o.text, uuid.NewString())
		return deliveredMsg{result: res, err: err}
	}
}

func (m *model) onDelivered(msg deliveredMsg) tea.Cmd {
	o := m.inflight
	m.inflight = nil
	if o == nil {
		return nil
	}
	var cmds []tea.Cmd
	err := msg.err
	if m.creating {
		m.creating = false
		if err == nil {
			m.sessionID = msg.created
			if m.squire != nil {
				if berr := m.squire.Bind(m.sessionID); berr != nil {
					err = fmt.Errorf("session %s started, but publishing the Squire binding failed: %w", shortID(m.sessionID), berr)
				}
			}
			cmds = append(cmds, m.startStream(), m.startPolling())
		}
		m.updatePlaceholder()
	}
	if o.reply != nil {
		o.reply <- err
	}
	if msg.err != nil {
		cmds = append(cmds, m.deliveryFailed(*o, msg.err))
		return tea.Batch(cmds...)
	}
	if err != nil {
		m.flash(err.Error(), toneWarn)
		cmds = append(cmds, m.flashTimer())
	} else if msg.result.Queued {
		m.flash("queued · delivered when the current turn ends", toneInfo)
		cmds = append(cmds, m.flashTimer())
	}
	return tea.Batch(append(cmds, m.pump())...)
}

// deliveryFailed stalls the outbox so later messages cannot overtake the
// failed one. Prompts from the control socket fail back to their sender,
// which owns retries; typed messages stay pending until the user retries.
func (m *model) deliveryFailed(o outgoing, err error) tea.Cmd {
	kept := m.outbox[:0]
	for _, p := range m.outbox {
		if p.reply != nil {
			p.reply <- fmt.Errorf("not delivered: an earlier message failed: %w", err)
			continue
		}
		kept = append(kept, p)
	}
	m.outbox = kept
	if o.reply == nil {
		if m.editor.Value() == "" && len(m.outbox) == 0 {
			m.editor.SetValue(o.text)
			m.editor.MoveToEnd()
		} else {
			m.outbox = append([]outgoing{o}, m.outbox...)
		}
	}
	m.stalled = len(m.outbox) > 0
	text := err.Error()
	if m.stalled {
		text += " · enter retries pending messages"
	}
	return m.flashCmd(text, toneError)
}

func (m *model) updatePlaceholder() {
	switch {
	case m.st.input != nil && len(m.st.input.Choices) > 0 && m.st.input.AllowFreeform:
		m.editor.Placeholder = "choose above or type an answer"
	case m.st.input != nil && len(m.st.input.Choices) > 0:
		m.editor.Placeholder = "choose an answer above"
	case m.st.input != nil:
		m.editor.Placeholder = "answer the question above"
	case m.sessionID == "":
		m.editor.Placeholder = "What should Orrery work on?"
	case m.st.running():
		m.editor.Placeholder = "queue a follow-up · esc interrupts"
	default:
		m.editor.Placeholder = "Send a message · / for commands · @ for files"
	}
}

func (m *model) workspace() string {
	if m.sessionSnapshot.WorkspacePath != "" {
		return m.sessionSnapshot.WorkspacePath
	}
	return m.opts.Create.Workspace
}

func (m *model) integrationLabel() string {
	if m.opts.SquireTaskID != "" {
		return "squire task " + shortID(m.opts.SquireTaskID)
	}
	if m.opts.Create.ExternalID != "" {
		return m.opts.Create.Integration + " " + shortID(m.opts.Create.ExternalID)
	}
	return ""
}

func (m *model) title() string {
	if m.sessionSnapshot.ID == "" {
		return ""
	}
	return clean(m.sessionSnapshot.DisplayTitle())
}

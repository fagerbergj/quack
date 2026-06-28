package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/stream"
)

// turn is one finished exchange in the transcript.
type turn struct {
	user      string
	answer    string
	dag       *dagState
	err       string
	cancelled bool
}

// ── messages (principle 2: one server event per msg) ─────────────────────────

type submitMsg struct{ text string }
type streamStartedMsg struct{ sub <-chan cli.SSEEvent }
type sseMsg struct {
	gen int // stream generation; stale events (gen != current) are dropped
	ev  cli.SSEEvent
}
type streamClosedMsg struct{ gen int }
type newChatMsg struct{ id string }
type cmdErrMsg struct{ err error }

// slashCommands drive both autocomplete and the dispatcher.
var slashCommands = []string{"/help", "/new", "/session", "/ui", "/inspect ", "/steer ", "/stop", "/node stop ", "/quit"}

// duckPuns rotate as the status line while the duck thinks — friendlier than a
// flat "thinking…".
var duckPuns = []string{
	"paddling furiously…",
	"ruffling some feathers…",
	"consulting the pond…",
	"preening the details…",
	"wading through it…",
	"diving deep…",
	"following the breadcrumbs…",
	"having a quack at it…",
}

// Model is the chat TUI. Update is a pure reducer; all I/O is in returned tea.Cmds.
type Model struct {
	ctx         context.Context
	client      *cli.Client
	chatID      string
	title       string
	serverLabel string

	turns        []turn
	streaming    bool
	cancelling   bool
	pending      string
	live         strings.Builder
	dag          *dagState
	runErr       string
	status       string
	overlay      string // "" | "help" | "session" | "node"
	inspectNode  string // node id shown when overlay == "node"
	initial      string
	pendingQuit  bool   // esc-once when idle; esc again quits
	pendingSteer string // guidance to resubmit once the cancelled run finishes
	tickN        int    // spinner ticks, drives pun rotation
	streamGen    int    // bumped per run; the pump tags events so a stale stream's tail is ignored

	sub       <-chan cli.SSEEvent
	cancelRun context.CancelFunc

	input textarea.Model
	vp    viewport.Model
	spin  spinner.Model
	md    *glamour.TermRenderer

	width, height int
	ready         bool
}

// New builds the chat model. history pre-populates the transcript (resume);
// initialPrompt, if set, is auto-sent on start; serverLabel is shown in the
// header (e.g. "local (in-process)" or a URL).
func New(ctx context.Context, c *cli.Client, chatID, title string, history []turn, initialPrompt, serverLabel string) Model {
	ta := textarea.New()
	ta.Placeholder = "Ask the duck…  (enter sends · \\ then enter for a newline · /help)"
	ta.Prompt = promptStyle.Render("│ ")
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// Enter is ours (send); newline is alt+enter / ctrl+j.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = runStyle

	return Model{
		ctx: ctx, client: c, chatID: chatID, title: title, serverLabel: serverLabel,
		turns: history, initial: initialPrompt, input: ta, spin: sp,
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, textarea.Blink}
	if m.initial != "" {
		init := m.initial
		cmds = append(cmds, func() tea.Msg { return submitMsg{init} })
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.relayout()
		m.refreshViewport()
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // wheel scroll
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.streaming {
			m.tickN++
			m.refreshViewport()
		}
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)

	case submitMsg:
		return m.startRun(msg.text)

	case streamStartedMsg:
		m.sub = msg.sub
		return m, waitForEvent(m.sub, m.streamGen)

	case sseMsg:
		if msg.gen != m.streamGen {
			return m, nil // stale tail of a previous run; drop it
		}
		return m.handleEvent(msg.ev)

	case streamClosedMsg:
		if msg.gen != m.streamGen {
			return m, nil
		}
		if m.streaming { // closed without a `done` → cancel or connection loss
			if m.runErr == "" && !m.cancelling {
				m.runErr = "connection lost — is the server still running?"
			}
			m.finishRun()
			cmd := m.consumeSteer()
			m.refreshViewport()
			return m, cmd
		}
		return m, nil

	case newChatMsg:
		m.chatID, m.title, m.turns = msg.id, "", nil
		m.live.Reset()
		m.dag, m.streaming = nil, false
		m.status = "started a new chat"
		m.refreshViewport()
		return m, nil

	case cmdErrMsg:
		m.status = msg.err.Error()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.relayout()
	return m, cmd
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k != "esc" {
		m.pendingQuit = false
	}
	// In the node-inspect overlay, up/down move between nodes.
	if m.overlay == "node" {
		switch k {
		case "up", "k":
			m.inspectMove(-1)
			m.refreshViewport()
			return m, nil
		case "down", "j":
			m.inspectMove(1)
			m.refreshViewport()
			return m, nil
		}
	}
	switch k {
	case "ctrl+o":
		d := m.currentDAG()
		if d == nil || len(d.nodes) == 0 {
			m.status = "no run to inspect"
			return m, nil
		}
		m.overlay = "node"
		m.inspectNode = defaultInspectNode(d)
		m.refreshViewport()
		return m, nil
	case "ctrl+c":
		if m.streaming {
			return m.cancelActive()
		}
		return m, tea.Quit
	case "esc":
		if m.overlay != "" {
			m.overlay = ""
			m.refreshViewport()
			return m, nil
		}
		if m.streaming {
			return m.cancelActive()
		}
		if m.pendingQuit {
			return m, tea.Quit
		}
		m.pendingQuit = true
		m.status = "press esc again to quit"
		return m, nil
	case "tab":
		if s := m.autocomplete(); s != "" {
			m.input.SetValue(s)
			m.input.CursorEnd()
		}
		return m, nil
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "enter":
		if m.streaming {
			m.status = "still thinking… ctrl+c or esc to stop"
			return m, nil
		}
		// Backslash-continuation: a trailing "\" + enter inserts a newline. Works in
		// every terminal (alt+enter/shift+enter aren't reliably delivered).
		if val := m.input.Value(); strings.HasSuffix(val, "\\") {
			m.input.SetHeight(inputMaxLines) // grow before the newline so content can't scroll off the top
			m.input.SetValue(val[:len(val)-1] + "\n")
			m.input.CursorEnd()
			m.relayout()
			return m, nil
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.Reset()
		m.status = ""
		m.overlay = ""
		if strings.HasPrefix(text, "/") {
			return m.slash(text)
		}
		return m, func() tea.Msg { return submitMsg{text} }
	}
	// Grow to max before the textarea handles the key: inserting a newline (or a
	// paste) while the box is too short makes its internal viewport scroll the top
	// line out of view, and the later SetHeight won't scroll it back. relayout
	// clamps the height back down to fit, with the offset still at the top.
	m.input.SetHeight(inputMaxLines)
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.relayout()
	return m, cmd
}

// autocomplete returns the completion for the current "/..." input: the single
// match, or the longest common prefix of several. "" if nothing to do.
func (m Model) autocomplete() string {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") || strings.Contains(val, "\n") {
		return ""
	}
	matches := m.slashMatches()
	switch len(matches) {
	case 0:
		return ""
	case 1:
		return matches[0]
	default:
		return longestCommonPrefix(matches)
	}
}

// slashMatches returns the slash commands that start with the current input.
func (m Model) slashMatches() []string {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") || strings.Contains(val, "\n") {
		return nil
	}
	var out []string
	for _, c := range slashCommands {
		if strings.HasPrefix(c, val) && c != val {
			out = append(out, c)
		}
	}
	return out
}

func (m Model) slash(text string) (tea.Model, tea.Cmd) {
	switch fields := strings.Fields(text); fields[0] {
	case "/help", "/?":
		m.overlay = toggle(m.overlay, "help")
		m.refreshViewport()
		return m, nil
	case "/session":
		m.overlay = toggle(m.overlay, "session")
		m.refreshViewport()
		return m, nil
	case "/ui", "/web":
		url := ""
		if m.client != nil {
			url = m.client.BaseURL
		}
		if url == "" {
			m.status = "no server URL to open"
			return m, nil
		}
		m.status = "opening " + url + " in your browser"
		return m, openBrowser(url)
	case "/quit", "/exit", "/q":
		return m, tea.Quit
	case "/new":
		c, ctx := m.client, m.ctx
		return m, func() tea.Msg {
			id, err := c.CreateChat(ctx, "")
			if err != nil {
				return cmdErrMsg{err}
			}
			return newChatMsg{id}
		}
	case "/steer":
		guidance := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		if guidance == "" {
			m.status = "usage: /steer <guidance>"
			return m, nil
		}
		if m.streaming {
			// Redirect: stop the current run, then resubmit the guidance so the
			// duck re-plans with full context. (Mid-node injection is a later,
			// server-side milestone; this is the safe, working form.)
			m.pendingSteer = guidance
			return m.cancelActive()
		}
		return m, func() tea.Msg { return submitMsg{guidance} }
	case "/stop":
		if m.streaming {
			return m.cancelActive()
		}
		m.status = "nothing running"
		return m, nil
	case "/inspect":
		if len(fields) < 2 {
			m.status = "usage: /inspect <node-id>"
			return m, nil
		}
		if d := m.currentDAG(); d == nil || d.node(fields[1]) == nil {
			m.status = "no node " + fields[1] + " in this run"
			return m, nil
		}
		m.overlay, m.inspectNode = "node", fields[1]
		m.refreshViewport()
		return m, nil
	case "/node":
		if len(fields) >= 3 && fields[1] == "stop" {
			nodeID := fields[2]
			c, ctx, id := m.client, m.ctx, m.chatID
			m.status = "stopping node " + nodeID + "…"
			return m, func() tea.Msg {
				_ = c.CancelNode(ctx, id, nodeID)
				return nil
			}
		}
		m.status = "usage: /node stop <node-id>"
		return m, nil
	default:
		m.status = "unknown command: " + fields[0] + "  (try /help)"
		return m, nil
	}
}

func (m Model) startRun(text string) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelRun = cancel
	m.streaming = true
	m.cancelling = false
	m.streamGen++ // new generation; the previous stream's late tail is now ignored
	m.pending = text
	m.runErr = ""
	m.live.Reset()
	m.dag = nil
	m.status = ""
	m.overlay = ""
	m.refreshViewport()
	c, id := m.client, m.chatID
	return m, func() tea.Msg { return streamStartedMsg{sub: c.Stream(ctx, id, text)} }
}

func (m Model) cancelActive() (tea.Model, tea.Cmd) {
	m.cancelling = true
	m.status = "cancelling…"
	if m.cancelRun != nil {
		m.cancelRun()
	}
	c, id := m.client, m.chatID
	return m, func() tea.Msg {
		_ = c.CancelRun(context.Background(), id)
		return nil
	}
}

func (m Model) handleEvent(ev cli.SSEEvent) (tea.Model, tea.Cmd) {
	switch ev.Name {
	case stream.EventDone:
		// `done` ends the run, but the server may still send late events after it
		// (the async chat_title is drained after the orchestrator's done). So
		// finalize once, then KEEP reading until the stream actually closes — this
		// is what lets the title land, and mirrors the React client (reads to EOF).
		if m.streaming {
			m.finishRun()
		}
		m.refreshViewport()
		return m, waitForEvent(m.sub, m.streamGen)
	case stream.EventError:
		if m.streaming {
			var d stream.ErrorData
			_ = json.Unmarshal(ev.Data, &d)
			m.runErr = d.Error
			m.finishRun()
		}
		m.refreshViewport()
		return m, waitForEvent(m.sub, m.streamGen)
	default:
		m.applyEvent(ev)
		m.refreshViewport()
		return m, waitForEvent(m.sub, m.streamGen)
	}
}

func (m *Model) applyEvent(ev cli.SSEEvent) {
	switch ev.Name {
	case stream.EventChatTitle:
		var d stream.ChatTitleData
		if json.Unmarshal(ev.Data, &d) == nil && d.Title != "" {
			m.title = d.Title
		}
	case stream.EventDagPlan:
		var d stream.DagPlanData
		if json.Unmarshal(ev.Data, &d) == nil {
			m.dag = newDAG(d)
		}
	case stream.EventNodeQueued:
		var d stream.NodeQueuedData
		_ = json.Unmarshal(ev.Data, &d)
		m.dagSet(d.NodeID, statusQueued)
	case stream.EventNodeStart:
		var d stream.NodeStartData
		_ = json.Unmarshal(ev.Data, &d)
		m.dagSet(d.NodeID, statusRunning)
	case stream.EventNodeDone:
		var d stream.NodeDoneData
		_ = json.Unmarshal(ev.Data, &d)
		m.dagSet(d.NodeID, statusDone)
		if m.dag != nil {
			m.dag.setOutput(d.NodeID, d.Output) // for deliver-mode terminal answer
		}
	case stream.EventNodeFailed:
		var d stream.NodeFailedData
		_ = json.Unmarshal(ev.Data, &d)
		if m.dag != nil {
			m.dag.fail(d.NodeID, d.Error)
		}
	case stream.EventAgentToken:
		var d stream.AgentTokenData
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID == "" {
			m.live.WriteString(d.Text)
		}
	case stream.EventAgentThinking:
		var d stream.AgentThinkingData
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID != "" && strings.TrimSpace(d.Text) != "" {
			m.dagActivity(d.NodeID, "💭 "+firstLine(d.Text))
		}
	case stream.EventAgentToolCall:
		var d stream.AgentToolCallData
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID != "" {
			m.dagActivity(d.NodeID, "🔧 "+d.Name+"("+argsSummary(d.Args)+")")
		}
	case stream.EventAgentToolResult:
		var d stream.AgentToolResultData
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID != "" {
			m.dagActivity(d.NodeID, "↳ "+resultSummary(d.Result))
		}
	}
}

func (m *Model) dagActivity(id, line string) {
	if m.dag != nil {
		m.dag.addActivity(id, line)
	}
}

// argsSummary renders tool args as a short one-liner for the activity feed.
func argsSummary(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncOneLine(string(b), 60)
}

func resultSummary(v any) string {
	switch t := v.(type) {
	case string:
		return truncOneLine(t, 80)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return truncOneLine(string(b), 80)
	}
}

func truncOneLine(s string, n int) string {
	s = strings.TrimSpace(firstLine(s))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (m *Model) dagSet(id string, s nodeStatus) {
	if m.dag != nil {
		m.dag.set(id, s)
	}
}

func (m *Model) finishRun() {
	answer := strings.TrimSpace(m.live.String())
	// Deliver mode (execute end_turn=true) streams the answer as node tokens, not
	// top-level ones, so the live buffer is empty — fall back to the terminal
	// node's output (what the server delivered + persisted).
	if answer == "" && m.dag != nil {
		answer = strings.TrimSpace(m.dag.terminalOutput())
	}
	t := turn{user: m.pending, answer: answer, dag: m.dag, err: m.runErr, cancelled: m.cancelling}
	m.turns = append(m.turns, t)
	m.live.Reset()
	m.dag = nil
	m.pending = ""
	m.streaming = false
	m.cancelling = false
	m.cancelRun = nil
	// Keep m.sub: the pump keeps draining until the stream closes, so a late
	// chat_title (sent after the orchestrator's done) still lands. The streamGen
	// guard drops the tail if a new run has started meanwhile.
	m.status = ""
	m.runErr = ""
}

// consumeSteer resubmits guidance queued by /steer once the cancelled run has
// finished (so two runs never overlap).
func (m *Model) consumeSteer() tea.Cmd {
	if m.pendingSteer == "" {
		return nil
	}
	g := m.pendingSteer
	m.pendingSteer = ""
	return func() tea.Msg { return submitMsg{g} }
}

func waitForEvent(sub <-chan cli.SSEEvent, gen int) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-sub
		if !ok {
			return streamClosedMsg{gen: gen}
		}
		return sseMsg{gen: gen, ev: ev}
	}
}

// ── layout & rendering ───────────────────────────────────────────────────────

func (m *Model) relayout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// Full width; height grows with content. Stock textarea.LineCount() counts only
	// logical lines (newlines), so a long wrapping line wouldn't expand the box —
	// inputLines counts WRAPPED rows. Capped so it can't eat the transcript;
	// beyond the cap the textarea scrolls internally.
	m.input.SetWidth(m.width)
	ih := m.inputLines()
	if ih < 1 {
		ih = 1
	}
	if ih > inputMaxLines {
		ih = inputMaxLines
	}
	m.input.SetHeight(ih)
	sugg := len(m.slashMatches())
	if sugg > 4 {
		sugg = 4
	}
	statusLines := 0
	if m.status != "" && !m.streaming {
		statusLines = 1
	}
	vpH := m.height - 1 /*header*/ - ih - 1 /*footer*/ - sugg - statusLines
	if vpH < 1 {
		vpH = 1
	}
	if !m.ready {
		m.vp = viewport.New(m.width, vpH)
		m.ready = true
		m.buildMD()
	} else {
		if m.vp.Width != m.width {
			m.vp.Width = m.width
			m.buildMD()
		}
		m.vp.Height = vpH
	}
}

const inputMaxLines = 12

// inputLines counts the visual rows the input needs — logical lines plus soft
// wrapping at the inner width — so a long line or a paste grows the box, not just
// explicit newlines (stock textarea.LineCount only counts newlines).
func (m *Model) inputLines() int {
	innerW := m.width - lipgloss.Width(m.input.Prompt)
	if innerW < 1 {
		innerW = m.width
	}
	n := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		w := lipgloss.Width(ln)
		rows := 1
		if innerW > 0 && w > innerW {
			rows = (w + innerW - 1) / innerW
		}
		n += rows
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (m *Model) buildMD() {
	w := m.vp.Width
	if w < 20 {
		w = 20
	}
	if r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(w-1)); err == nil {
		m.md = r
	}
}

func (m *Model) refreshViewport() {
	if !m.ready {
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(m.transcript())
	if atBottom || m.streaming {
		m.vp.GotoBottom()
	}
}

func (m Model) View() string {
	if !m.ready {
		return "starting…"
	}
	header := headerStyle.Render("🦆 quack")
	if m.title != "" {
		header += "  " + m.title
	}
	if m.serverLabel != "" {
		header += "  " + faintStyle.Render("· "+m.serverLabel)
	}

	footer := faintStyle.Render("enter send · \\+enter newline · /help · ctrl+c quit")
	if m.streaming {
		footer = faintStyle.Render("ctrl+c / esc stop · pgup/pgdn or wheel scroll")
	}

	parts := []string{header, m.vp.View()}

	if m.streaming {
		parts = append(parts, m.spin.View()+" "+runStyle.Render(m.pun()))
	} else {
		if m.status != "" {
			parts = append(parts, mutedStyle.Render(m.status))
		}
		parts = append(parts, m.input.View())
		// Command hints go BELOW the input box (telesense-style).
		if sugg := m.slashMatches(); len(sugg) > 0 {
			parts = append(parts, faintStyle.Render("  "+strings.Join(capN(sugg, 4), "  ")))
		}
	}
	parts = append(parts, footer)
	return strings.Join(parts, "\n")
}

func (m Model) pun() string {
	return duckPuns[(m.tickN/20)%len(duckPuns)]
}

func (m Model) transcript() string {
	switch m.overlay {
	case "help":
		return helpText()
	case "session":
		return m.sessionText()
	case "node":
		return m.nodeDetailText()
	}
	width := m.vp.Width
	var b strings.Builder
	if len(m.turns) == 0 && !m.streaming {
		b.WriteString(mutedStyle.Render("Say something to the duck. It thinks, researches, and talks back.\n"))
	}
	for _, t := range m.turns {
		b.WriteString(m.renderTurn(t, "", nil, width))
	}
	if m.streaming {
		live := turn{user: m.pending, answer: strings.TrimSpace(m.live.String())}
		b.WriteString(m.renderTurn(live, m.spin.View(), m.dag, width))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) renderTurn(t turn, spin string, liveDAG *dagState, width int) string {
	plain := lipgloss.NewStyle().Width(maxInt(width-2, 10))
	var b strings.Builder
	b.WriteString(youStyle.Render("You") + "\n")
	b.WriteString(plain.Render(t.user) + "\n\n")

	dag := t.dag
	if liveDAG != nil {
		dag = liveDAG
	}
	if dag != nil {
		b.WriteString(dagBox.Render(dag.render(spin, width-4)) + "\n\n")
	}

	b.WriteString(duckStyle.Render("Duck") + "\n")
	switch {
	case t.err != "":
		if t.answer != "" {
			b.WriteString(m.markdown(t.answer) + "\n")
		}
		b.WriteString(errStyle.Render("⚠ "+t.err) + "\n")
	case t.cancelled:
		if t.answer != "" {
			b.WriteString(m.markdown(t.answer) + "\n")
		}
		b.WriteString(mutedStyle.Render("⊘ cancelled") + "\n")
	case t.answer == "" && spin != "":
		b.WriteString(mutedStyle.Render("…") + "\n")
	case spin != "":
		// streaming: raw text (markdown mid-stream renders broken)
		b.WriteString(plain.Render(t.answer) + "\n")
	default:
		b.WriteString(m.markdown(t.answer) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// markdown renders finished answers; falls back to the raw text on any error.
func (m Model) markdown(s string) string {
	if m.md == nil || strings.TrimSpace(s) == "" {
		return s
	}
	out, err := m.md.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func (m Model) sessionText() string {
	server := m.serverLabel
	if server == "" {
		server = "(unknown)"
	}
	title := m.title
	if title == "" {
		title = "(untitled)"
	}
	lines := []string{
		headerStyle.Render("Session"),
		"",
		"  " + mutedStyle.Render("server ") + server,
		"  " + mutedStyle.Render("chat   ") + m.chatID,
		"  " + mutedStyle.Render("title  ") + title,
		"  " + mutedStyle.Render("turns  ") + fmt.Sprintf("%d", len(m.turns)),
		"",
		mutedStyle.Render("  esc to close"),
	}
	return strings.Join(lines, "\n")
}

// inspectMove changes the inspected node to the prev/next in plan order.
func (m *Model) inspectMove(delta int) {
	d := m.currentDAG()
	if d == nil {
		return
	}
	i, ok := d.index[m.inspectNode]
	if !ok {
		i = 0
	} else {
		i += delta
	}
	if i < 0 {
		i = 0
	}
	if i >= len(d.nodes) {
		i = len(d.nodes) - 1
	}
	m.inspectNode = d.nodes[i].id
}

// defaultInspectNode picks the node to open: the running one, else the first.
func defaultInspectNode(d *dagState) string {
	for _, n := range d.nodes {
		if n.status == statusRunning {
			return n.id
		}
	}
	return d.nodes[0].id
}

// currentDAG returns the DAG to inspect: the in-flight run's, else the most
// recent finished turn's.
func (m Model) currentDAG() *dagState {
	if m.dag != nil {
		return m.dag
	}
	for i := len(m.turns) - 1; i >= 0; i-- {
		if m.turns[i].dag != nil {
			return m.turns[i].dag
		}
	}
	return nil
}

// nodeDetailText renders a node's full activity feed (thinking, tool calls,
// results) — the drill-in, and the basis for steering.
func (m Model) nodeDetailText() string {
	d := m.currentDAG()
	if d == nil {
		return mutedStyle.Render("no run to inspect")
	}
	n := d.node(m.inspectNode)
	if n == nil {
		return mutedStyle.Render("node " + m.inspectNode + " not found")
	}
	label := n.agent
	if label == "" {
		label = n.id
	}
	var b strings.Builder
	b.WriteString(headerStyle.Render("Node "+n.id) + "  " + mutedStyle.Render(label) + "\n")
	if task := strings.TrimSpace(n.task); task != "" {
		b.WriteString(mutedStyle.Render(task) + "\n")
	}
	b.WriteString("\n")
	if len(n.activity) == 0 {
		b.WriteString(mutedStyle.Render("(no activity yet)") + "\n")
	}
	for _, a := range n.activity {
		b.WriteString(a + "\n")
	}
	b.WriteString("\n" + mutedStyle.Render("↑/↓ other nodes · esc to close"))
	return b.String()
}

func helpText() string {
	lines := []string{
		headerStyle.Render("Commands"),
		"",
		"  " + promptStyle.Render("/help") + "            toggle this help",
		"  " + promptStyle.Render("/session") + "         show session details",
		"  " + promptStyle.Render("/ui") + "              open the web UI in your browser",
		"  " + promptStyle.Render("/new") + "             start a new chat",
		"  " + promptStyle.Render("/inspect <id>") + "    drill into a node's tool calls + thinking",
		"  " + promptStyle.Render("ctrl+o") + "           inspect nodes (↑/↓ to move between them)",
		"  " + promptStyle.Render("/steer <text>") + "    stop and redirect the duck with new guidance",
		"  " + promptStyle.Render("/stop") + "            cancel the running turn",
		"  " + promptStyle.Render("/node stop <id>") + "  stop one running node",
		"  " + promptStyle.Render("/quit") + "            exit",
		"",
		mutedStyle.Render("  enter send · alt+enter newline · tab complete · ctrl+c/esc stop or quit"),
		mutedStyle.Render("  pgup/pgdn or mouse wheel to scroll · esc to close"),
	}
	return strings.Join(lines, "\n")
}

// openBrowser opens url in the user's default browser (best-effort, non-blocking).
func openBrowser(url string) tea.Cmd {
	return func() tea.Msg {
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", url)
		case "windows":
			c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			c = exec.Command("xdg-open", url)
		}
		_ = c.Start()
		return nil
	}
}

func toggle(cur, want string) string {
	if cur == want {
		return ""
	}
	return want
}

func capN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

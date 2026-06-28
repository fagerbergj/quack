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
type sseMsg struct{ ev cli.SSEEvent }
type streamClosedMsg struct{}
type newChatMsg struct{ id string }
type cmdErrMsg struct{ err error }

// slashCommands drive both autocomplete and the dispatcher.
var slashCommands = []string{"/help", "/new", "/session", "/ui", "/stop", "/node stop ", "/quit"}

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

	turns       []turn
	streaming   bool
	cancelling  bool
	pending     string
	live        strings.Builder
	dag         *dagState
	runErr      string
	status      string
	overlay     string // "" | "help" | "session"
	initial     string
	pendingQuit bool // esc-once when idle; esc again quits
	tickN       int  // spinner ticks, drives pun rotation

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
	ta.Placeholder = "Ask the duck…  (enter to send · alt+enter newline · /help)"
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
		return m, waitForEvent(m.sub)

	case sseMsg:
		return m.handleEvent(msg.ev)

	case streamClosedMsg:
		if m.streaming {
			if m.runErr == "" && !m.cancelling {
				m.runErr = "connection lost — is the server still running?"
			}
			m.finishRun()
			m.refreshViewport()
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
	switch k {
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
	case "/stop":
		if m.streaming {
			return m.cancelActive()
		}
		m.status = "nothing running"
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
		m.finishRun()
		m.refreshViewport()
		return m, nil
	case stream.EventError:
		var d stream.ErrorData
		_ = json.Unmarshal(ev.Data, &d)
		m.runErr = d.Error
		m.finishRun()
		m.refreshViewport()
		return m, nil
	default:
		m.applyEvent(ev)
		m.refreshViewport()
		return m, waitForEvent(m.sub)
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
	}
}

func (m *Model) dagSet(id string, s nodeStatus) {
	if m.dag != nil {
		m.dag.set(id, s)
	}
}

func (m *Model) finishRun() {
	t := turn{user: m.pending, answer: strings.TrimSpace(m.live.String()), dag: m.dag, err: m.runErr, cancelled: m.cancelling}
	m.turns = append(m.turns, t)
	m.live.Reset()
	m.dag = nil
	m.pending = ""
	m.streaming = false
	m.cancelling = false
	m.cancelRun = nil
	m.sub = nil
	m.status = ""
	m.runErr = ""
}

func waitForEvent(sub <-chan cli.SSEEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-sub
		if !ok {
			return streamClosedMsg{}
		}
		return sseMsg{ev}
	}
}

// ── layout & rendering ───────────────────────────────────────────────────────

func (m *Model) relayout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// Full width; height grows with content — LineCount counts wrapped lines too,
	// so a long single line (or a paste) expands the box, capped so it can't eat
	// the transcript (beyond the cap the textarea scrolls internally).
	m.input.SetWidth(m.width)
	ih := m.input.LineCount()
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

	footer := faintStyle.Render("enter send · alt+enter newline · /help · ctrl+c quit")
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

func helpText() string {
	lines := []string{
		headerStyle.Render("Commands"),
		"",
		"  " + promptStyle.Render("/help") + "            toggle this help",
		"  " + promptStyle.Render("/session") + "         show session details",
		"  " + promptStyle.Render("/ui") + "              open the web UI in your browser",
		"  " + promptStyle.Render("/new") + "             start a new chat",
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

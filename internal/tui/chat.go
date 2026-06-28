package tui

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/stream"
)

// turn is one finished exchange in the transcript.
type turn struct {
	user      string
	answer    string
	dag       *dagState
	err       string // run error (connection lost, server error), if any
	cancelled bool   // user stopped the run
}

// ── messages (principle 2: one server event per msg) ─────────────────────────

type submitMsg struct{ text string } // user (or initial prompt) submitted text
type streamStartedMsg struct{ sub <-chan cli.SSEEvent }
type sseMsg struct{ ev cli.SSEEvent } // one decoded server event
type streamClosedMsg struct{}         // stream channel closed (cancel / drop / no explicit done)
type newChatMsg struct{ id string }
type cmdErrMsg struct{ err error } // a side-effect command failed

// Model is the chat TUI. Update is a pure reducer; all I/O is in returned tea.Cmds.
type Model struct {
	ctx    context.Context
	client *cli.Client
	chatID string
	title  string

	turns      []turn
	streaming  bool
	cancelling bool
	pending    string          // user text of the in-flight turn
	live       strings.Builder // accumulated top-level answer tokens
	dag        *dagState       // current run's DAG (nil until dag_plan)
	runErr     string          // error for the in-flight run
	status     string          // transient status/notice line
	initial    string          // prompt to auto-send on start

	sub       <-chan cli.SSEEvent
	cancelRun context.CancelFunc

	input textinput.Model
	vp    viewport.Model
	spin  spinner.Model

	width, height int
	ready         bool
	showHelp      bool
}

// New builds the chat model. history pre-populates the transcript (resume);
// initialPrompt, if set, is auto-sent once the program starts.
func New(ctx context.Context, c *cli.Client, chatID, title string, history []turn, initialPrompt string) Model {
	in := textinput.New()
	in.Placeholder = "Ask the duck…  (enter to send · /help for commands)"
	in.Prompt = ""
	in.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = runStyle

	return Model{
		ctx: ctx, client: c, chatID: chatID, title: title,
		turns: history, initial: initialPrompt, input: in, spin: sp,
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, textinput.Blink}
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
		m.layout()
		m.refreshViewport()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.streaming {
			m.refreshViewport() // animate running-node icons
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

	// Default: route to the input (typing).
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.streaming {
			return m.cancelActive()
		}
		return m, tea.Quit
	case "esc":
		if m.showHelp {
			m.showHelp = false
			m.refreshViewport()
			return m, nil
		}
		if m.streaming {
			return m.cancelActive()
		}
		return m, nil
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "enter":
		if m.streaming {
			m.status = "still thinking… press ctrl+c or /stop to cancel"
			return m, nil
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.Reset()
		m.status = ""
		if strings.HasPrefix(text, "/") {
			return m.slash(text)
		}
		return m, func() tea.Msg { return submitMsg{text} }
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// slash dispatches in-TUI commands (mirrors the cobra verbs).
func (m Model) slash(text string) (tea.Model, tea.Cmd) {
	switch fields := strings.Fields(text); fields[0] {
	case "/help", "/?":
		m.showHelp = !m.showHelp
		m.refreshViewport()
		return m, nil
	case "/quit", "/exit", "/q":
		return m, tea.Quit
	case "/new":
		c, ctx := m.client, m.ctx
		return m, func() tea.Msg {
			newID, err := c.CreateChat(ctx, "")
			if err != nil {
				return cmdErrMsg{err}
			}
			return newChatMsg{newID}
		}
	case "/stop":
		if m.streaming {
			return m.cancelActive()
		}
		m.status = "nothing running"
		return m, nil
	default:
		m.status = "unknown command: " + fields[0] + "  (try /help)"
		return m, nil
	}
}

// startRun begins streaming a turn.
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
	m.refreshViewport()
	c, id := m.client, m.chatID
	return m, func() tea.Msg { return streamStartedMsg{sub: c.Stream(ctx, id, text)} }
}

// cancelActive stops the in-flight run: cancel the local stream ctx (closes the
// channel → streamClosedMsg finalizes) and tell the server to cancel too.
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

// applyEvent folds one non-terminal event into the live run state.
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
			m.live.WriteString(d.Text) // top-level answer only; node tokens stay in the DAG
		}
	}
}

func (m *Model) dagSet(id string, s nodeStatus) {
	if m.dag != nil {
		m.dag.set(id, s)
	}
}

// finishRun moves the in-flight run into the transcript and resets live state.
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

// waitForEvent pulls exactly one event off the stream; closed channel → done.
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

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.input.Width = m.width - 4
	vpHeight := m.height - 4 // header(1) + input(1) + footer(1) + spacing(1)
	if vpHeight < 1 {
		vpHeight = 1
	}
	if !m.ready {
		m.vp = viewport.New(m.width, vpHeight)
		m.ready = true
	} else {
		m.vp.Width = m.width
		m.vp.Height = vpHeight
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
		header += "  " + mutedStyle.Render(m.title)
	}

	var footer string
	if m.streaming {
		footer = faintStyle.Render("ctrl+c stop · pgup/pgdn scroll")
	} else {
		footer = faintStyle.Render("enter send · /help · ctrl+c quit")
	}

	var inputLine string
	if m.streaming {
		st := m.status
		if st == "" {
			st = "thinking…"
		}
		inputLine = m.spin.View() + " " + mutedStyle.Render(st)
	} else {
		inputLine = promptStyle.Render("› ") + m.input.View()
		if m.status != "" {
			inputLine = mutedStyle.Render(m.status) + "\n" + inputLine
		}
	}

	return strings.Join([]string{header, m.vp.View(), inputLine, footer}, "\n")
}

// transcript renders the full scrollback: finished turns + the in-flight run.
func (m Model) transcript() string {
	if m.showHelp {
		return helpText()
	}
	width := m.vp.Width
	var b strings.Builder
	if len(m.turns) == 0 && !m.streaming {
		b.WriteString(mutedStyle.Render("Say something to the duck. It thinks, researches, and talks back.\n"))
	}
	for _, t := range m.turns {
		b.WriteString(renderTurn(t, "", nil, width))
	}
	if m.streaming {
		live := turn{user: m.pending, answer: strings.TrimSpace(m.live.String())}
		b.WriteString(renderTurn(live, m.spin.View(), m.dag, width))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTurn renders one exchange. spin/liveDAG are only set for the in-flight
// turn (so running nodes animate); finished turns carry their own t.dag.
func renderTurn(t turn, spin string, liveDAG *dagState, width int) string {
	wrap := lipgloss.NewStyle().Width(maxInt(width-2, 10))
	var b strings.Builder
	b.WriteString(youStyle.Render("You") + "\n")
	b.WriteString(wrap.Render(t.user) + "\n\n")

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
		body := t.answer
		if body != "" {
			b.WriteString(wrap.Render(body) + "\n")
		}
		b.WriteString(errStyle.Render("⚠ "+t.err) + "\n")
	case t.cancelled:
		if t.answer != "" {
			b.WriteString(wrap.Render(t.answer) + "\n")
		}
		b.WriteString(mutedStyle.Render("⊘ cancelled") + "\n")
	case t.answer == "" && spin != "":
		b.WriteString(mutedStyle.Render("…") + "\n")
	default:
		b.WriteString(wrap.Render(t.answer) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

func helpText() string {
	lines := []string{
		headerStyle.Render("Commands"),
		"",
		"  " + promptStyle.Render("/help") + "   toggle this help",
		"  " + promptStyle.Render("/new") + "    start a new chat",
		"  " + promptStyle.Render("/stop") + "   cancel the running turn",
		"  " + promptStyle.Render("/quit") + "   exit",
		"",
		mutedStyle.Render("  enter send · ctrl+c cancel/quit · pgup/pgdn scroll · esc close help"),
	}
	return strings.Join(lines, "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

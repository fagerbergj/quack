# Quack CLI/TUI - implementation recipes

Copy-ready skeletons for the decisions in `SKILL.md`.
Read the matching section when you implement that piece.
These are scaffolds to adapt, not drop-in final code.

---

## 1. The Bubble Tea model (Init / Update / View)

`internal/tui/chat.go`. `Update` is a pure reducer - every branch returns a new model and maybe a `tea.Cmd`.
No I/O in the body.

```go
package tui

import tea "github.com/charmbracelet/bubbletea"

type Model struct {
	client   *cli.Client      // from internal/cli - never the other way around
	chatID   string
	events   []stream.Event   // accumulated SSE events (the same union the React app renders)
	input    textinput.Model  // bubbles component
	spinner  spinner.Model
	streaming bool
	err      error
}

func New(c *cli.Client, chatID string) Model { /* init components */ }

func (m Model) Init() tea.Cmd { return tea.Batch(m.spinner.Tick, waitForEvent(m.sub)) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			text := m.input.Value()
			m.input.Reset()
			m.streaming = true
			return m, m.submit(text)        // returns a tea.Cmd - the POST happens there, not here
		}
	case sseMsg:                            // one server event (principle 2)
		m.events = append(m.events, msg.ev)
		if msg.ev.Type == "done" {
			m.streaming = false
			return m, nil                  // stop pumping
		}
		return m, waitForEvent(m.sub)      // re-issue: pull the next event
	case errMsg:
		m.err = msg.err
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) View() string { /* lipgloss-styled render of m.events + input + spinner */ }
```

Run it from the command layer: `tea.NewProgram(New(client, id), tea.WithAltScreen()).Run()`.

---

## 2. The SSE pump - a self-re-issuing tea.Cmd (principle 2)

One event per message; `Update` re-issues to get the next. `m.sub` is a channel the client fills from the HTTP/SSE response body.

```go
type sseMsg struct{ ev stream.Event }
type errMsg struct{ err error }

func waitForEvent(sub <-chan stream.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-sub
		if !ok {
			return sseMsg{ev: stream.Event{Type: "done"}}
		}
		return sseMsg{ev: ev}
	}
}
```

The client (`internal/cli`) owns the actual `http.Client` + SSE parse, decoding the wire vocabulary into `stream.Event` and pushing onto `sub`.
The TUI never touches `net/http`.

---

## 3. Tier-1 reducer test (no framework - the workhorse)

`internal/tui/chat_test.go`.
Pure `Update`, so just feed msgs and assert.

```go
func TestUpdate_DoneStopsStreaming(t *testing.T) {
	m := Model{streaming: true}
	got, cmd := m.Update(sseMsg{ev: stream.Event{Type: "done"}})
	if got.(Model).streaming {
		t.Fatal("done event must clear streaming")
	}
	if cmd != nil {
		t.Fatal("done must not re-issue the pump") // no further waitForEvent
	}
}

func TestUpdate_NodeEventReissuesPump(t *testing.T) {
	m := Model{streaming: true, sub: make(chan stream.Event)}
	_, cmd := m.Update(sseMsg{ev: stream.Event{Type: "node_start"}})
	if cmd == nil {
		t.Fatal("a non-terminal event must re-issue waitForEvent to keep pumping")
	}
}
```

Assert on the returned `cmd`'s presence/type, not by executing it (executing it would block on the channel - that's tier 2's job).

---

## 4. Tier-2 golden render test (teatest - keep few)

`internal/tui/chat_golden_test.go`.
Fixed term size or the golden is nondeterministic.

```go
import "github.com/charmbracelet/x/exp/teatest"

func TestTUI_RendersRun(t *testing.T) {
	m := New(stubClient(t), "chat1")     // stub client replays a canned SSE stream
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))

	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // drive it
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))

	out, _ := io.ReadAll(tm.FinalOutput(t))
	teatest.RequireEqualOutput(t, out)     // diffs against testdata/*.golden
}
```

Regenerate after an intended visual change: `go test -run TUI -update ./internal/tui/...`, then eyeball the `.golden` diff (a Lipgloss tweak rewrites all of them - confirm it's what you meant).

---

## 5. The `server init` wizard (Huh) - test the YAML, not the keystrokes (principle 5)

`internal/cli/init.go`.
Huh owns the form; your code maps answers → config.

```go
import "github.com/charmbracelet/huh"

func RunInit() (*config.File, error) {
	var topology string // "embedded" | "managed" | "external"
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Stores topology").
			Options(
				huh.NewOption("Embedded - SQLite, no containers", "embedded"),
				huh.NewOption("Managed - compose Postgres + Qdrant", "managed"),
				huh.NewOption("External - BYO via QUACK_DATABASE_URL/QUACK_QDRANT_URL", "external"),
			).Value(&topology),
	))
	if err := form.Run(); err != nil {
		return nil, err
	}
	return configFor(topology), nil // pure func: answers -> *config.File
}
```

Test the pure mapper, not the form:

```go
func TestConfigFor_Embedded(t *testing.T) {
	cfg := configFor("embedded")
	if cfg.Session.Kind != "sqlite" || cfg.Memory.Kind != "sqlite" {
		t.Fatal("embedded topology must select sqlite for session + memory")
	}
	// round-trip through the real loader so an invalid emit fails here, not at `serve`
	if _, err := config.Load(cfg.Marshal()); err != nil {
		t.Fatalf("emitted quack.yaml does not load: %v", err)
	}
}
```

---

## 6. Print mode - no TUI, no ANSI when piped (principle 4)

`internal/cli/print.go`.
Plain stdout; never starts a `tea.Program`.

```go
func Print(ctx context.Context, c *Client, prompt string) error {
	sub, err := c.Stream(ctx, prompt) // same client the TUI uses
	if err != nil {
		return err
	}
	for ev := range sub {
		switch ev.Type {
		case "agent_token":
			fmt.Print(ev.Text)        // raw text - pipes into jq/CI cleanly
		case "node_start":
			fmt.Fprintf(os.Stderr, "▶ %s\n", ev.NodeID) // status to stderr, content to stdout
		}
	}
	return nil
}
```

Color: rely on Lipgloss/`NO_COLOR` auto-detect for any styled bits, but the validation loop's `quack -p … | cat` check is the real gate - stdout must be escape-free when not a tty.

---

## 7. cobra wiring - thin commands (principle 6, gotcha 7)

`cmd/quack/main.go`.
Commands parse + dispatch; logic lives in `internal/cli`/`internal/tui`.

```go
func main() {
	root := &cobra.Command{Use: "quack", RunE: runInteractiveOrPrint} // bare `quack` / `quack -p`
	var prompt string
	root.Flags().StringVarP(&prompt, "print", "p", "", "one-shot print mode (no TUI)")

	chat := &cobra.Command{Use: "chat"}
	chat.AddCommand(cmdChatNew(), cmdChatResume(), cmdChatList() /* … */)
	node := &cobra.Command{Use: "node"} // quack chat node stop|cancel|restart|steer
	node.AddCommand(cmdNodeStop(), cmdNodeSteer())
	chat.AddCommand(node)

	server := &cobra.Command{Use: "server"}           // serve|init|stop|use|add|list
	server.AddCommand(cmdServe(), cmdServerInit())     // cmdServe folds in old cmd/server
	root.AddCommand(chat, server, cmdAPI(), cmdVersion())
	_ = root.Execute()
}
```

Each `cmdX()` returns a `*cobra.Command` whose `RunE` does one thing: build a `cli.Client`, call the one shared action func (the same func the in-TUI slash-command calls), print/stream the result.
Keep the business logic out of `RunE` so it's unit-testable without cobra.

---

## Dependency add (one-time)

```bash
go get github.com/charmbracelet/bubbletea github.com/charmbracelet/bubbles \
       github.com/charmbracelet/lipgloss github.com/charmbracelet/huh \
       github.com/charmbracelet/x/exp/teatest github.com/spf13/cobra
```

Keep the build pure-Go (`CGO_ENABLED=0`) - none of these need cgo, which keeps M10 cross-compile trivial.

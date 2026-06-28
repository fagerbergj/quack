package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fagerbergj/quack/internal/cli"
)

func newTestModel() Model {
	return New(context.Background(), nil, "c1", "", nil, "", "")
}

func ev(name, data string) cli.SSEEvent {
	return cli.SSEEvent{Name: name, Data: json.RawMessage(data)}
}

func TestUpdate_DoneFinishesRun(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.pending = "hi"
	m.live.WriteString("answer")
	got, cmd := m.Update(sseMsg{ev: ev("done", "{}")})
	gm := got.(Model)
	if gm.streaming {
		t.Error("done must clear streaming")
	}
	if cmd != nil {
		t.Error("done must not re-issue the pump")
	}
	if len(gm.turns) != 1 || gm.turns[0].answer != "answer" || gm.turns[0].user != "hi" {
		t.Errorf("done must move the run into a turn, got %+v", gm.turns)
	}
}

func TestUpdate_NonTerminalReissuesPump(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.sub = make(chan cli.SSEEvent)
	_, cmd := m.Update(sseMsg{ev: ev("node_start", `{"node_id":"a","agent":"r"}`)})
	if cmd == nil {
		t.Error("a non-terminal event must re-issue waitForEvent")
	}
}

func TestUpdate_ErrorEventMarksTurn(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.pending = "q"
	got, cmd := m.Update(sseMsg{ev: ev("error", `{"error":"boom"}`)})
	gm := got.(Model)
	if gm.streaming {
		t.Error("error must clear streaming")
	}
	if cmd != nil {
		t.Error("error is terminal — no re-issue")
	}
	if len(gm.turns) != 1 || gm.turns[0].err != "boom" {
		t.Errorf("error must annotate the turn, got %+v", gm.turns)
	}
}

func TestUpdate_StreamClosedIsConnectionLost(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.pending = "q"
	got, _ := m.Update(streamClosedMsg{})
	gm := got.(Model)
	if gm.streaming {
		t.Error("a closed stream must end streaming")
	}
	if len(gm.turns) != 1 || gm.turns[0].err == "" {
		t.Errorf("an unexpected close is a connection error, got %+v", gm.turns)
	}
}

func TestUpdate_CancelIsNotAnError(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.pending = "q"
	m.cancelling = true // user pressed ctrl+c
	got, _ := m.Update(streamClosedMsg{})
	gm := got.(Model)
	if len(gm.turns) != 1 || gm.turns[0].err != "" || !gm.turns[0].cancelled {
		t.Errorf("a cancelled run is cancelled, not errored: %+v", gm.turns)
	}
}

func TestUpdate_CtrlCStreamingCancels_IdleQuits(t *testing.T) {
	// streaming → cancel (stay in TUI), returns a cmd
	m := newTestModel()
	m.streaming = true
	m.cancelRun = func() {}
	got, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !got.(Model).cancelling {
		t.Error("ctrl+c while streaming must cancel")
	}
	if cmd == nil {
		t.Error("cancel should issue a server-cancel cmd")
	}

	// idle → quit
	m2 := newTestModel()
	_, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd2 == nil {
		t.Fatal("ctrl+c while idle must quit")
	}
	if _, ok := cmd2().(tea.QuitMsg); !ok {
		t.Error("idle ctrl+c must return tea.Quit")
	}
}

func TestUpdate_EnterSubmitsNonEmpty(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("  hello  ")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter with text must produce a submit cmd")
	}
	msg := cmd()
	sm, ok := msg.(submitMsg)
	if !ok || sm.text != "hello" {
		t.Errorf("submit must carry the trimmed text, got %#v", msg)
	}

	// empty input → no submit
	m2 := newTestModel()
	m2.input.SetValue("   ")
	_, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd2 != nil {
		t.Error("empty input must not submit")
	}
}

func TestUpdate_EnterWhileStreamingDoesNotSubmit(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.input.SetValue("queued?")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("enter while streaming must not start a second run")
	}
}

func TestSlash_HelpToggles(t *testing.T) {
	m := newTestModel()
	got, _ := m.slash("/help")
	if got.(Model).overlay != "help" {
		t.Error("/help must show help")
	}
}

func TestEscEscQuitsWhenIdle(t *testing.T) {
	m := newTestModel()
	got, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	gm := got.(Model)
	if !gm.pendingQuit || cmd != nil {
		t.Fatal("first idle esc should arm quit, not quit")
	}
	_, cmd2 := gm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd2 == nil {
		t.Fatal("second esc should quit")
	}
	if _, ok := cmd2().(tea.QuitMsg); !ok {
		t.Error("second esc must return tea.Quit")
	}
}

func TestEscWhileStreamingCancels(t *testing.T) {
	m := newTestModel()
	m.streaming = true
	m.cancelRun = func() {}
	got, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !got.(Model).cancelling || cmd == nil {
		t.Error("esc while streaming must cancel, not arm quit")
	}
}

func TestAutocomplete(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/se")
	if got := m.autocomplete(); got != "/session" {
		t.Errorf("autocomplete /se = %q, want /session", got)
	}
	m.input.SetValue("/x")
	if got := m.autocomplete(); got != "" {
		t.Errorf("autocomplete of no-match = %q, want empty", got)
	}
}

func TestSlash_SessionOverlay(t *testing.T) {
	m := newTestModel()
	got, _ := m.slash("/session")
	if got.(Model).overlay != "session" {
		t.Error("/session should open the session overlay")
	}
}

func TestSlash_NodeStopUsage(t *testing.T) {
	m := newTestModel()
	// Bad form → usage hint, no command.
	got, cmd := m.slash("/node")
	if cmd != nil || !strings.Contains(got.(Model).status, "usage") {
		t.Errorf("/node without args should show usage, status=%q", got.(Model).status)
	}
	// Well-formed → issues a cmd (the cancel call).
	_, cmd2 := m.slash("/node stop n1")
	if cmd2 == nil {
		t.Error("/node stop <id> should issue a cancel cmd")
	}
}

func TestSlash_Unknown(t *testing.T) {
	m := newTestModel()
	got, _ := m.slash("/nope")
	if got.(Model).status == "" {
		t.Error("unknown command must set a status hint")
	}
}

func TestApplyEvent_TokensAndDAG(t *testing.T) {
	m := newTestModel()
	m.applyEvent(ev("dag_plan", `{"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"do","depends_on":[]}],"edges":[]}`))
	if m.dag == nil || len(m.dag.nodes) != 1 {
		t.Fatalf("dag_plan must build the DAG, got %+v", m.dag)
	}
	m.applyEvent(ev("node_start", `{"node_id":"a","agent":"r"}`))
	if m.dag.nodes[0].status != statusRunning {
		t.Error("node_start must mark the node running")
	}
	m.applyEvent(ev("agent_token", `{"text":"top "}`))                // no node_id → live answer
	m.applyEvent(ev("agent_token", `{"node_id":"a","text":"inner"}`)) // node-scoped → ignored
	if m.live.String() != "top " {
		t.Errorf("only top-level tokens feed the answer, got %q", m.live.String())
	}
}

func TestUpdate_NewChatResets(t *testing.T) {
	m := newTestModel()
	m.turns = []turn{{user: "old"}}
	got, _ := m.Update(newChatMsg{id: "c2"})
	gm := got.(Model)
	if gm.chatID != "c2" || len(gm.turns) != 0 {
		t.Errorf("new chat must switch id and clear turns, got id=%s turns=%d", gm.chatID, len(gm.turns))
	}
}

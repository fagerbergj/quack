package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// sized returns a model laid out at a fixed size with its viewport populated —
// the deterministic way to assert View() content without teatest's ANSI golden.
func sized(m Model) Model {
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return mm.(Model)
}

func TestView_TranscriptAndDAG(t *testing.T) {
	d := sampleDAG()
	d.set("a", statusDone)
	d.set("b", statusRunning)
	m := sized(New(context.Background(), nil, "c1", "Ducks", []turn{
		{user: "hello", answer: "hi there"},
		{user: "research ducks", answer: "Ducks are great.", dag: d},
	}, "", ""))

	v := m.View()
	for _, want := range []string{"quack", "Ducks", "You", "hello", "Duck", "Ducks are great.", "researcher", "synthesizer", "✓"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
}

// Streaming must not fight a manual scroll-up: once the user scrolls off the
// bottom, an incoming token (refreshViewport) keeps their position instead of
// yanking them back down. Regression for "scroll disabled while a DAG runs".
func TestStickyScroll_StreamingKeepsScrollPosition(t *testing.T) {
	var hist []turn
	for i := 0; i < 40; i++ {
		hist = append(hist, turn{user: "question", answer: "a sufficiently long answer line"})
	}
	m := sized(New(context.Background(), nil, "c", "", hist, "", ""))
	m.streaming = true

	// User scrolls to the top to read history.
	m.vp.GotoTop()
	if m.vp.AtBottom() {
		t.Fatal("precondition: 40 turns should overflow a 24-row viewport")
	}

	// A streaming token arrives.
	m.refreshViewport()
	if m.vp.AtBottom() {
		t.Error("streaming refresh yanked the viewport to the bottom despite scroll-up")
	}

	// But while pinned at the bottom, streaming still auto-follows.
	m.vp.GotoBottom()
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Error("streaming should keep following when already at the bottom")
	}
}

func TestView_EmptyStateGuidesUser(t *testing.T) {
	m := sized(New(context.Background(), nil, "c1", "", nil, "", ""))
	if !strings.Contains(m.View(), "Say something to the duck") {
		t.Errorf("empty chat should invite input:\n%s", m.View())
	}
}

func TestView_ErrorAndCancelledTurns(t *testing.T) {
	m := sized(New(context.Background(), nil, "c1", "", []turn{
		{user: "q1", err: "connection lost"},
		{user: "q2", answer: "partial", cancelled: true},
	}, "", ""))
	v := m.View()
	if !strings.Contains(v, "connection lost") {
		t.Error("errored turn must show the error")
	}
	if !strings.Contains(v, "cancelled") {
		t.Error("cancelled turn must show it was cancelled")
	}
}

func TestView_HelpOverlay(t *testing.T) {
	m := sized(New(context.Background(), nil, "c1", "", nil, "", ""))
	got, _ := m.slash("/help")
	v := got.(Model).View()
	for _, want := range []string{"Commands", "/new", "/stop", "/quit"} {
		if !strings.Contains(v, want) {
			t.Errorf("help overlay missing %q", want)
		}
	}
}

func TestView_ServerLabelAndSession(t *testing.T) {
	m := sized(New(context.Background(), nil, "chat-123", "", nil, "", "local (in-process)"))
	if !strings.Contains(m.View(), "local (in-process)") {
		t.Error("header should show the server label")
	}
	got, _ := m.slash("/session")
	v := got.(Model).View()
	for _, want := range []string{"Session", "chat-123", "local (in-process)"} {
		if !strings.Contains(v, want) {
			t.Errorf("/session overlay missing %q", want)
		}
	}
}

func TestView_NotReadyBeforeSize(t *testing.T) {
	m := New(context.Background(), nil, "c1", "", nil, "", "")
	if m.View() != "starting…" {
		t.Errorf("before a size, View should be a placeholder, got %q", m.View())
	}
}

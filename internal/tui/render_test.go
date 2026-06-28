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
	}, ""))

	v := m.View()
	for _, want := range []string{"quack", "Ducks", "You", "hello", "Duck", "Ducks are great.", "researcher", "synthesizer", "✓"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
}

func TestView_EmptyStateGuidesUser(t *testing.T) {
	m := sized(New(context.Background(), nil, "c1", "", nil, ""))
	if !strings.Contains(m.View(), "Say something to the duck") {
		t.Errorf("empty chat should invite input:\n%s", m.View())
	}
}

func TestView_ErrorAndCancelledTurns(t *testing.T) {
	m := sized(New(context.Background(), nil, "c1", "", []turn{
		{user: "q1", err: "connection lost"},
		{user: "q2", answer: "partial", cancelled: true},
	}, ""))
	v := m.View()
	if !strings.Contains(v, "connection lost") {
		t.Error("errored turn must show the error")
	}
	if !strings.Contains(v, "cancelled") {
		t.Error("cancelled turn must show it was cancelled")
	}
}

func TestView_HelpOverlay(t *testing.T) {
	m := sized(New(context.Background(), nil, "c1", "", nil, ""))
	got, _ := m.slash("/help")
	v := got.(Model).View()
	for _, want := range []string{"Commands", "/new", "/stop", "/quit"} {
		if !strings.Contains(v, want) {
			t.Errorf("help overlay missing %q", want)
		}
	}
}

func TestView_NotReadyBeforeSize(t *testing.T) {
	m := New(context.Background(), nil, "c1", "", nil, "")
	if m.View() != "starting…" {
		t.Errorf("before a size, View should be a placeholder, got %q", m.View())
	}
}

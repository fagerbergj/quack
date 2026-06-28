// Package tui is quack's interactive terminal chat — a Bubble Tea program that
// streams a run from the server (the same SSE vocabulary the React app renders)
// and draws the transcript, the live answer, and the DAG. It imports the client
// from internal/cli; the client never imports back (keeps pipe/CI paths
// terminal-dependency free).
package tui

import "github.com/charmbracelet/lipgloss"

// The rubber-duck palette, shared with the CLI wizard so the whole surface reads
// on-brand.
const (
	duck  = lipgloss.Color("#FFD23F") // rubber-duck yellow
	beak  = lipgloss.Color("#F5A623") // orange
	muted = lipgloss.Color("#6B6B6B")
	faint = lipgloss.Color("#444444")
	green = lipgloss.Color("#02BF87")
	red   = lipgloss.Color("#ED567A")
	blue  = lipgloss.Color("#7571F9")
)

var (
	headerStyle = lipgloss.NewStyle().Foreground(duck).Bold(true)
	youStyle    = lipgloss.NewStyle().Foreground(blue).Bold(true)
	duckStyle   = lipgloss.NewStyle().Foreground(duck).Bold(true)
	mutedStyle  = lipgloss.NewStyle().Foreground(muted)
	faintStyle  = lipgloss.NewStyle().Foreground(faint)
	errStyle    = lipgloss.NewStyle().Foreground(red)
	okStyle     = lipgloss.NewStyle().Foreground(green)
	runStyle    = lipgloss.NewStyle().Foreground(beak)
	promptStyle = lipgloss.NewStyle().Foreground(beak).Bold(true)

	// dagBox frames the DAG panel with a faint left rule.
	dagBox = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(faint).
		PaddingLeft(1)
)

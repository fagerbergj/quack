package wizard

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// duckTheme is a rubber-duck-themed huh theme: a yellow focused border (the
// duck) with an orange "beak" selector and cursor. Built on ThemeBase so the
// layout/spacing stays conventional; only the accent colors move. Used by the
// init wizard so `quack init` reads as on-brand, not a generic form.
func duckTheme() *huh.Theme {
	t := huh.ThemeBase()

	const (
		duck    = "#FFD23F" // rubber-duck yellow
		beak    = "#F5A623" // orange beak
		blurred = "#6B6B6B" // muted grey for unfocused fields
	)

	// Focused: a yellow left border + yellow titles/cursor; the select
	// selector is an orange beak (›).
	t.Focused.Base = lipgloss.NewStyle().
		PaddingLeft(1).
		BorderStyle(lipgloss.ThickBorder()).
		BorderLeft(true).
		BorderForeground(lipgloss.Color(duck))
	t.Focused.Card = t.Focused.Base
	t.Focused.Title = lipgloss.NewStyle().Foreground(lipgloss.Color(duck)).Bold(true)
	t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("› ")
	t.Focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("› ")
	t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color(duck)).SetString("[•] ")
	t.Focused.NextIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("→")
	t.Focused.PrevIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("←")
	t.Focused.TextInput.Cursor = lipgloss.NewStyle().Foreground(lipgloss.Color(beak))
	t.Focused.TextInput.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(blurred))
	t.Focused.FocusedButton = t.Focused.FocusedButton.
		Foreground(lipgloss.Color("#000")).Background(lipgloss.Color(duck))

	// Blurred: muted, so the focused field pops as the one yellow duck.
	t.Blurred = t.Focused
	t.Blurred.Base = t.Blurred.Base.BorderForeground(lipgloss.Color(blurred))
	t.Blurred.Title = lipgloss.NewStyle().Foreground(lipgloss.Color(blurred))
	t.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.SelectedPrefix = lipgloss.NewStyle().SetString("[ ] ")
	t.Blurred.TextInput.Cursor = lipgloss.NewStyle().Foreground(lipgloss.Color(blurred))
	t.Blurred.TextInput.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("#444"))

	return t
}

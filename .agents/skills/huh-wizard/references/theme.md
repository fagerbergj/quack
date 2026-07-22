# huh-wizard — theme recipe

The rubber-duck theme + the `runForm` wrapper. v1 (`github.com/charmbracelet/huh v1.0.0`). Adapt, don't drop in blind. For the form patterns, see `forms.md`.

---

## The theme (rubber-duck, on `huh.ThemeBase()`)

`internal/wizard/theme.go`. Build once, route every form through `runForm`. Start from `ThemeBase()` so layout/spacing stay conventional and only the accent colors move.

```go
package wizard

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

func duckTheme() *huh.Theme {
	t := huh.ThemeBase()
	const (
		duck    = "#FFD23F" // rubber-duck yellow
		beak    = "#F5A623" // orange beak
		blurred = "#6B6B6B" // muted grey for unfocused fields
	)
	t.Focused.Base = lipgloss.NewStyle().
		PaddingLeft(1).
		BorderStyle(lipgloss.NormalBorder()). // a thin left rule reads cleaner than ThickBorder's heavy bar
		BorderLeft(true).
		BorderForeground(lipgloss.Color(duck))
	t.Focused.Card = t.Focused.Base
	t.Focused.Title = lipgloss.NewStyle().Foreground(lipgloss.Color(duck)).Bold(true)
	t.Focused.Description = lipgloss.NewStyle().Foreground(lipgloss.Color(blurred)) // field subtitle

	// Group title/description = the section header. ThemeBase leaves these as a
	// zero style (only ThemeCharm et al. set them), so they render blank unless
	// set explicitly here. Setting t.Focused.Title does NOT flow through to them.
	t.Group.Title = lipgloss.NewStyle().Foreground(lipgloss.Color(duck)).Bold(true)
	t.Group.Description = lipgloss.NewStyle().Foreground(lipgloss.Color(blurred))
	t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("› ")
	t.Focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("› ")
	t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color(duck)).SetString("[•] ")
	t.Focused.NextIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("→")
	t.Focused.PrevIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color(beak)).SetString("←")
	t.Focused.TextInput.Cursor = lipgloss.NewStyle().Foreground(lipgloss.Color(beak))
	t.Focused.TextInput.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(blurred))
	t.Focused.FocusedButton = t.Focused.FocusedButton.
		Foreground(lipgloss.Color("#000")).Background(lipgloss.Color(duck))

	// Blurred: muted, so the one focused field pops as the yellow duck.
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
```

## The runForm wrapper

Every form goes through this so the theme is consistent and you can't forget it. It also forces an alt-screen: huh renders inline by default, so a tall group (a long `/models` list) scrolls the section title off the top of the terminal. `WithProgramOptions` *replaces* huh's defaults, so re-supply the stderr output (keeps stdout pipeable) and focus reporting.

```go
import tea "github.com/charmbracelet/bubbletea"

func runForm(f *huh.Form) error {
	return f.WithTheme(duckTheme()).
		WithProgramOptions(
			tea.WithOutput(os.Stderr),
			tea.WithReportFocus(),
			tea.WithAltScreen(),
		).Run()
}
```

## Notes

- A custom theme is taste + risk: the ANSI escapes leak into golden snapshots, so don't theme a surface you golden-test without accounting for it. Keep the default theme if there's no brand call.
- Blurred styling matters as much as focused: muting unfocused fields is what makes the one focused field read as the subject. It's also what lets you stack several selects in one group (see `forms.md` → specialist models) — blurred selects collapse to just their value.

# huh-wizard — theme & form recipes

Copy-ready skeletons for the decisions in `SKILL.md`. v1 (`github.com/charmbracelet/huh v1.0.0`).
Adapt, don't drop in blind.

---

## The theme (rubber-duck, on `huh.ThemeBase()`)

`internal/wizard/theme.go`. Build once, route every form through `runForm`.

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

Every form goes through this so the theme is consistent and you can't forget it:

```go
func runForm(f *huh.Form) error { return f.WithTheme(duckTheme()).Run() }
```

## Section-grouped forms (back-nav within a section)

Title + description go on the **first group** (Form has no `.Title()` in v1).

```go
// askProvider — one group, three fields, navigable together. API key masked.
func askProvider(ctx context.Context, a *cli.InitAnswers) error {
	var kind string
	return runForm(huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("LLM provider").
			Options(huh.NewOption("OpenAI-compatible", "openai")).
			Value(&kind),
		huh.NewInput().Title("Endpoint").Placeholder("http://localhost:11436/v1").Value(&a.Endpoint),
		huh.NewInput().Title("API key").Placeholder("blank if none").
			EchoMode(huh.EchoModePassword).Value(&a.APIKey),
	).Title("LLM provider").Description("How quack reaches its model server")))
}

// askModels — five groups in one form → shift+tab returns to an earlier role.
func askModels(ctx context.Context, a *cli.InitAnswers) error {
	models, err := cli.ListModels(ctx, a.Endpoint, a.APIKey)
	manual := err != nil
	none := huh.NewOption("None — disable", "")
	return runForm(huh.NewForm(
		huh.NewGroup(selectOrInput(manual, "Main chat model", modelOptions(models, a.MainModel), &a.MainModel)).
			Title("Models").Description("The model quack reasons and plans with"),
		huh.NewGroup(specialistSelect("Judge model", models, &a.JudgeModel, none)),
		huh.NewGroup(specialistSelect("Embedding model", models, &a.EmbedModel, none)),
		huh.NewGroup(specialistSelect("Vision model", models, &a.VisionModel, none)),
		huh.NewGroup(specialistSelect("Audio model", models, &a.AudioModel, none)),
	))
}
```

## The final review/confirm gate

A `Note` summary + a `Confirm` group. The user can back out of the write from here.

```go
func askConfirm(a *cli.InitAnswers, outPath string) error {
	summary := fmt.Sprintf("endpoint %s • main %s • judge %s", a.Endpoint, a.MainModel, noneLabel(a.JudgeModel))
	var ok bool
	return runForm(huh.NewForm(
		huh.NewGroup(huh.NewNote().Title("Review").Description(summary)),
		huh.NewGroup(huh.NewConfirm().Title(fmt.Sprintf("Write %s?", outPath)).Value(&ok)),
	))
}
```

## Tolerate stale pre-fills (select)

A `Select` won't highlight a value missing from its options. When you pre-fill from env and the value
might not be in the discovered list, add it as an explicit option:

```go
func specialistSelect(title string, models []string, val *string, none huh.Option[string]) huh.Field {
	if len(models) == 0 {
		return huh.NewInput().Title(title + " (blank for none)").Value(val) // manual fallback
	}
	opts := append([]huh.Option[string]{none}, modelOptions(models, *val)...)
	if *val != "" && !slices.Contains(models, *val) {
		opts = append(opts, huh.NewOption(*val, *val)) // stale env → still pre-selects
	}
	return huh.NewSelect[string]().Title(title).Options(opts...).Value(val)
}
```

## Default a multi-select ON

Pre-seed the bound slice with the option values — not a bool:

```go
feats := []string{}
if a.WebSearch { feats = append(feats, "search") }
if a.WebFetch  { feats = append(feats, "fetch")  }
huh.NewMultiSelect[string]().
	Options(huh.NewOption("Web search", "search"), huh.NewOption("Web fetch", "fetch")).
	Value(&feats) // the pre-seeded feats show checked
```

## v1 field/option/group modifiers (verified at v1.0.0)

| On | Method | Notes |
|---|---|---|
| `Input` | `.Title .Value .Placeholder .EchoMode .Suggestions .Key` | `EchoModePassword` masks secrets |
| `Select[T]` | `.Title .Options .Value .Key` | option values via `huh.NewOption(label, val)` |
| `MultiSelect[T]` | `.Title .Options .Value` | pre-selected via the bound slice |
| `Confirm` | `.Title .Value` | bool pointer |
| `Note` | `.Title .Description` | non-interactive summary block |
| `Option[T]` | `.Selected(bool)` | pre-select a MultiSelect option directly |
| `Group` | `.Title .Description .WithHeight(n)` | **section header lives here in v1** |
| `Form` | `.WithTheme .Run` | **no `.Title()` in v1** — use the first group |

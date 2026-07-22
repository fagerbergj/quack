# huh-wizard — form recipes

Copy-ready form skeletons for the decisions in `SKILL.md`. v1 (`github.com/charmbracelet/huh v1.0.0`). Adapt, don't drop in blind. For the rubber-duck theme + `runForm` wrapper, see `theme.md`.

---

## The two-form shape (one I/O break, everything else merged)

`server init` is **two** forms: the provider (which yields the endpoint), then — after the `/models` fetch — one big form for models + features + stores + review. The fetch is the only reason to split; see SKILL idiom #1.

```go
func ServerInit(ctx context.Context, outPath string, force bool) error {
	a := cli.InitAnswers{WebSearch: true, WebFetch: true}
	cli.PrefillFromEnv(&a)

	if err := askProvider(ctx, &a); err != nil { return err } // form 1 → endpoint/key
	models, manual := discoverModels(ctx, &a)                  // I/O break: fetch /models, apply heuristics

	feats := featureList(&a) // seed multi-select from the bool defaults
	var ok bool
	groups := modelGroups(&a, models, manual)            // form 2: everything else, one form
	groups = append(groups, featuresGroup(&feats))
	groups = append(groups, storeGroups(&a, &feats)...)
	groups = append(groups, reviewGroup(&a, &feats, outPath, &ok))
	if err := runForm(huh.NewForm(groups...)); err != nil { return err }

	a.WebSearch = slices.Contains(feats, "search")
	a.WebFetch = slices.Contains(feats, "fetch")
	if !ok { fmt.Println("Aborted — nothing written."); return nil } // HONOR the confirm
	// ... write file ...
}
```

Title + description go on the **first group** of a section (Form has no `.Title()` in v1).

```go
// askProvider — one group, three fields, navigable together. API key masked.
func askProvider(ctx context.Context, a *cli.InitAnswers) error {
	var kind string
	return runForm(huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("LLM provider").
			Options(huh.NewOption("OpenAI-compatible", "openai")).Value(&kind),
		huh.NewInput().Title("Endpoint").Placeholder("http://localhost:11436/v1").Value(&a.Endpoint),
		huh.NewInput().Title("API key").Placeholder("blank if none").
			EchoMode(huh.EchoModePassword).Value(&a.APIKey),
	).Title("LLM provider").Description("How quack reaches its model server")))
}

// modelGroups — ONE role per group (its own screen). A blurred huh select still
// renders its full option list, so stacking several overflows the screen and the
// group viewport scrolls. The group title is the header → the field has no title.
func modelGroups(a *cli.InitAnswers, models []string, manual bool) []*huh.Group {
	none := huh.NewOption("None — disable", "")
	return []*huh.Group{
		huh.NewGroup(selectOrInput(manual, modelOptions(models), &a.MainModel)).
			Title("Main model").Description("The model quack reasons and plans with"),
		huh.NewGroup(specialistSelect(models, &a.JudgeModel, none)).
			Title("Judge model").Description("Trust gate — None disables it"),
		huh.NewGroup(specialistSelect(models, &a.EmbedModel, none)).
			Title("Embedding model").Description("Semantic memory — None disables it"),
		// ...vision, audio, same shape
	}
}

// selectOrInput / specialistSelect — no field title (group title is the header),
// no .Height (the option list stays static; the cursor moves through it).
func selectOrInput(manual bool, opts []huh.Option[string], val *string) huh.Field {
	if manual || len(opts) == 0 {
		return huh.NewInput().Value(val)
	}
	return huh.NewSelect[string]().Options(opts...).Value(val)
}
```

## Conditional store groups (`WithHideFunc`, same form)

Sections gated on an earlier answer stay in the form and hide themselves — no second form, back-nav intact. Gate the *emit* on the same flags so a hidden group's default never lands in the output.

Give **every** group a `.Title()` — it's the section header. A conditional group with only field titles reads as headerless next to the ones that have a header. `storeGroup` sets the group title to the store name and uses generic field titles ("Backend"/"URL") so the name isn't printed twice.

```go
func storeGroups(a *cli.InitAnswers, feats *[]string) []*huh.Group {
	session := storeGroup("Session storage", ...).Description("...") // always shown; title set inside storeGroup
	memory  := storeGroup("Memory store", ...).WithHideFunc(func() bool { return a.EmbedModel == "" })
	search  := storeGroup("Web search backend", ...).WithHideFunc(func() bool { return !slices.Contains(*feats, "search") })
	fetch   := storeGroup("Web fetch backend", ...).WithHideFunc(func() bool { return !slices.Contains(*feats, "fetch") })
	return []*huh.Group{session, memory, search, fetch}
}

func storeGroup(title string, kinds []string, kind, url *string, defKind string) *huh.Group {
	*kind = defKind // url left blank: placeholder shows the per-kind default; emit fills it
	return huh.NewGroup(
		huh.NewSelect[string]().Title("Backend").Options(toOptions(kinds)...).Value(kind),
		huh.NewInput().Title("URL").
			PlaceholderFunc(func() string { // re-fires when *kind changes (binding hashed by value)
				if d := cli.DefaultBackendURL(*kind); d != "" {
					return d + " (default)"
				}
				return "none needed"
			}, kind).
			Value(url),
	).Title(title) // group title = section header
}
```

**A field default that tracks another field's selection** = `PlaceholderFunc(fn, binding)`. huh hashes the binding *by value* (via `hashstructure`, which dereferences pointers), so passing the `*string` you bound to the select re-runs the func whenever the selected kind changes. There's no `ValueFunc`, so the pattern is: leave the value blank, show the live default as the *placeholder*, and have the consumer treat blank as "use the default for this kind" (here `cli.DefaultBackendURL` is the single source of truth for both the placeholder and the emitter). Accepting the default is then just pressing enter.

## The final review/confirm gate (one screen, live summary, honored confirm)

Note + Confirm in **one group** → the summary and the Yes/No render together (separate groups = two screens). The `Note` is non-interactive so navigation skips to the confirm. `Note.DescriptionFunc` recomputes after a back-up-and-edit; the static `Description(string)` would show stale values. And `ok` must be **checked** by the caller — `Confirm` only records it.

```go
func reviewGroup(a *cli.InitAnswers, feats *[]string, outPath string, ok *bool) *huh.Group {
	return huh.NewGroup(
		huh.NewNote().DescriptionFunc(func() string { return summarize(a, feats) }, a), // recomputes live
		huh.NewConfirm().Title("Write " + outPath + "?").Value(ok),
	).Title("Review").Description("Confirm before writing " + outPath)
}
```

## Tolerate stale pre-fills (select)

A `Select` won't highlight a value missing from its options. When you pre-fill from env and the value might not be in the discovered list, add it as an explicit option:

```go
func specialistSelect(models []string, val *string, none huh.Option[string]) huh.Field {
	if len(models) == 0 {
		return huh.NewInput().Placeholder("blank for none").Value(val) // manual fallback; group title labels it
	}
	opts := append([]huh.Option[string]{none}, modelOptions(models)...)
	if *val != "" && !slices.Contains(models, *val) {
		opts = append(opts, huh.NewOption(*val, *val)) // stale env → still pre-selects
	}
	return huh.NewSelect[string]().Options(opts...).Value(val) // no title/Height — see modelGroups
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
| `Select[T]` | `.Title .Options .Value .Key .Filtering(b) .Height(n) .OptionsFunc(f,bind)` | `Filtering`/`Height` are situational (filter box grabs focus; Height makes options a scrolling window) — omit unless the list is huge; `OptionsFunc` computes options from a binding |
| `MultiSelect[T]` | `.Title .Options .Value` | pre-selected via the bound slice |
| `Confirm` | `.Title .Value` | bool pointer — **caller must check it; it doesn't abort** |
| `Note` | `.Title .Description .TitleFunc(f,bind) .DescriptionFunc(f,bind)` | `*Func` recompute live (stale-summary fix) |
| `Option[T]` | `.Selected(bool)` | pre-select a MultiSelect option directly |
| `Group` | `.Title .Description .WithHeight(n) .WithHideFunc(func() bool)` | section header + **live conditional visibility** (group-level only) |
| `Form` | `.WithTheme .Run` | **no `.Title()` in v1** — use the first group |

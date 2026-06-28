---
name: huh-wizard
description: |
  How to build polished multi-stage interactive wizards with Charm `huh` (v1) — the form library quack's
  `quack init` / `quack server init` use. Covers the idioms that separate a generic form from a
  deliberate, navigable wizard: multi-group forms for built-in back-navigation, WithHideFunc for
  answer-dependent sections (split a second form only across an I/O boundary like a /models fetch), group
  titles/descriptions as section headers, placeholders + EchoModePassword + Filtering/Height + .Key() on
  fields, a final review/confirm group with a live DescriptionFunc summary, and a custom theme. Includes
  the gotchas that bite (Form has no .Title() in v1 — use the first group; separate forms per question
  kill back-nav; a Confirm value is never checked unless you check it; embed paths can't climb out of a
  package). Use when adding
  or restyling any `huh.NewForm` wizard, debugging a form that can't go back, or theming a quack CLI
  surface. Do NOT use for Bubble Tea screens (that's quack-cli) or non-interactive CLI paths.
---

# Skill: huh-wizard

## Overview

How to build beautiful, navigable multi-stage wizards with `github.com/charmbracelet/huh` v1 — the form
library behind quack's onboarding (`quack init`, `quack server init`). The job is a *wizard*: several
sections of questions, the user can move forward and back, with a themed, designed feel rather than a
flat form. This skill captures the idioms learned from huh's official examples (`git`, `ssh-form`,
`multiple-groups`) plus the v1-vs-v2 gotchas that bit during the quack wizard build.

## When to Use

- Building or restyling any `huh.NewForm` wizard (onboarding, `init`, config generation).
- A form where the user wants to go **back** to an earlier question.
- Theming a quack CLI interactive surface (rubber-duck theme lives at `internal/wizard/theme.go`).
- Debugging "I can't return to the previous question" or "Form has no method Title".

## When NOT to Use

- **Bubble Tea screens** (full TUI chat, streaming views) — that's the `quack-cli` skill. Huh is for
  *forms* (finite, field-driven); Bubble Tea is for *apps* (event-loop, streaming).
- **Non-interactive CLI paths** (`-p` print mode, `api` passthrough) — they must not start a form.
- **Deterministic tool execution** — a wizard collects answers; if it needs to *do* something typed,
  use `write-tool`.

## The idioms (what separates a wizard from a form)

### 1. One multi-group form per section → built-in back-navigation

Huh's back-navigation (shift+tab / ←) only works **within a single form**, across its groups. The
cardinal mistake is a **separate form per question** — once a form `Run()`s, you can't return to it.

```go
// ✅ one form, multiple groups → back-nav between roles
huh.NewForm(
  huh.NewGroup(field("Main model", &main)),
  huh.NewGroup(field("Judge model", &judge)),
  huh.NewGroup(field("Embed model", &embed)),
)

// ❌ three forms → no way back once each finishes
huh.NewForm(huh.NewGroup(field("Main", &main))).Run()
huh.NewForm(huh.NewGroup(field("Judge", &judge))).Run()
huh.NewForm(huh.NewGroup(field("Embed", &embed))).Run()
```

**The dependency cut is about I/O, not answers.** A form's *fields* are fixed up front, but a group's
*visibility* is not — `Group.WithHideFunc(func() bool {...})` re-evaluates live as the user navigates,
reading the bound values of earlier fields. So a section gated on an earlier **answer** (store questions
gated on the features multi-select; the memory store gated on whether an embed model was chosen) belongs
in the **same** form via `WithHideFunc` — keeping back-nav intact. (See idiom #6.)

You only need a *second* form when a section depends on **I/O** that must happen between sections — e.g.
`quack server init` fetches `/models` from the endpoint before it can offer models as select options.
That fetch can't run mid-form, so the provider form runs first, then the fetch, then one big form for
everything else. Accept the single back-nav wall there; it falls on a natural break (re-entering the
endpoint *should* re-fetch). Earlier guidance here said any answer-dependency forces a second form —
that was wrong; only an I/O-dependency does.

### 2. Group `.Title()` + `.Description()` = section headers

In v1, **`Form` has no `.Title()`** (that's v2). The section header is **each group's** title +
description. A form with untitled groups reads as a flat list with no context — and it's easy to title
the obvious first group but forget the conditional ones (a `WithHideFunc` store group), which then look
headerless next to the rest. Give *every* group a title; put the store name on the group and use generic
field titles ("Backend"/"URL") so the name isn't printed twice.

```go
huh.NewForm(
  huh.NewGroup(
    huh.NewInput().Title("Endpoint").Value(&endpoint),
    huh.NewInput().Title("API key").Value(&key),
  ).Title("LLM provider").Description("How quack reaches its model server"),
  ...
)
```

### 3. Field polish

- `.Placeholder("http://localhost:11436/v1")` — hint the expected format on every input.
- `.EchoMode(huh.EchoModePassword)` — mask secrets (API keys). Plain inputs echo the key visibly.
- `.Key("endpoint")` — name a field so `form.GetString("endpoint")` works after (and for introspection).
  Pointer-based `Value(&x)` also works; keys are cleaner for post-form access.
- `.Suggestions(types)` — for a typed field with known values (e.g. commit types, or model IDs as a
  manual fallback when `/models` failed).
- `.PlaceholderFunc(fn, binding)` — a **default that tracks another field's selection** (e.g. a URL whose
  default depends on the selected backend kind). There's no `ValueFunc`, so leave the value blank, show
  the live default as the placeholder, and have the consumer treat blank as "use the default for this
  kind." The binding is hashed by value (pointers dereferenced), so pass the `*string` the other field is
  bound to and the func re-fires when it changes. Keep one source of truth for the default that both the
  placeholder and the consumer call.
- **One select per group; let the option list be static.** Two anti-patterns bite here, both learned the
  hard way:
  - *Don't `.Height(n)` a select to "cap" it.* Height makes the options a scrolling window — arrow past
    the bottom and the earlier options slide out of view with no indicator you can scroll back. The user
    expects the list to stay put and the cursor to move. Omit Height; with an alt-screen there's room.
  - *Don't stack several selects in one group.* A blurred huh `Select` still renders its **full** option
    list (it does *not* collapse to its value), so N selects stack to N full lists — taller than the
    screen, and now the *group* viewport scrolls. Give each role its own group (its own screen). More
    enter-presses, but every screen is static and back-nav still works.
  - With one field per group, the **group title is the header** — give the field an empty title (or, in a
    two-field group like a store's kind+url, generic field titles like "Backend"/"URL") so the name isn't
    printed twice.

### 4. End with a confirm group — live summary, and *honor* the value

A wizard should review before it writes. End with a `Note` summary + a `huh.NewConfirm()` gate **in the
same group** — separate groups put the answers and the Yes/No on two different screens. A `Note` is
non-interactive, so navigation skips it and lands on the confirm; both render on one page.

Two things the first cut got wrong:

- **Use `DescriptionFunc(fn, bindings)`, not `Description(string)`.** A static summary string is captured
  before the form runs — if the user backs up and changes an answer, the review still shows the *old*
  values. `DescriptionFunc` recomputes on every render; pass the answers struct as `bindings` so it
  refreshes. (`huh.NewNote()` has `TitleFunc`/`DescriptionFunc`; most fields have the `*Func` pair.)
- **Actually check the confirm's bound bool.** `Confirm.Value(&ok)` only *records* the choice; nothing
  aborts for you. After `Run()`, `if !ok { return }` — it's trivially easy to write the file regardless
  and never notice, because the happy path looks identical.

```go
huh.NewForm(
  huh.NewGroup( // one group → summary + confirm on a single screen
    huh.NewNote().DescriptionFunc(func() string { return summarize(a) }, a),
    huh.NewConfirm().Title("Write quack.yaml?").Value(&ok),
  ).Title("Review"),
)
// ... after Run():
if !ok { fmt.Println("Aborted — nothing written."); return nil }
```

### 5. Conditional sections without leaving the form (`WithHideFunc`)

`Group.WithHideFunc(func() bool {...})` is how a section appears/disappears based on an earlier answer
*inside the same form* — the fix for the "second form per dependency" mistake (idiom #1). The func runs
on every navigation; return `true` to hide. Read the **live** bound values, not a snapshot.

```go
memory := storeGroup(...).WithHideFunc(func() bool { return a.EmbedModel == "" })          // no embedder → no memory store
search := storeGroup(...).WithHideFunc(func() bool { return !slices.Contains(feats, "search") }) // feature off → no backend
```

Two things to know:

- **It's `Group`-level only — fields have no `WithHideFunc`.** To toggle a *single* field, give it its
  own group. (Don't over-split, though — see the "stack selects" note in idiom #3.)
- **A hidden group still holds its bound value.** Gate the *emit* on the same condition (`if a.WebSearch
  {...}`) so a hidden group's leftover default never lands in the output. Belt-and-suspenders with the
  hide func.

### 6. Theme it (one helper, applied to every form)

Build the theme once (`huh.ThemeBase()` + accent overrides) and route every form through a `runForm`
helper so the look is consistent. See `references/theme.md` for the full rubber-duck theme +
the `runForm` wrapper. Keep the default theme if there's no brand call — a custom theme is taste + risk
(ANSI in golden tests).

**`ThemeBase()` leaves `Group.Title`/`Group.Description` unstyled.** Unlike `ThemeCharm` (and the other
stock themes), `ThemeBase` never assigns `t.Group.Title`/`t.Group.Description` — they're a zero style, so
group *section headers* render blank even though field titles (which use `t.Focused.Title`) look fine.
If you build on `ThemeBase` and your `.Title()` on a group shows nothing, set `t.Group.Title` and
`t.Group.Description` explicitly. (Setting `t.Focused.Title` does **not** flow through — `ThemeCharm`
copies Focused→Group by value at the end of its setup, so a later override of Focused doesn't update Group.)

## Gotchas

- **`Form.Title()` does not exist in v1.** The examples on `charm.land/huh/v2` use `.Title()` on the
  form; our pinned `github.com/charmbracelet/huh v1.0.0` has it only on `Group`. Compile error:
  `*huh.Form has no field or method Title`. Fix: put the title on the first group.
- **huh renders inline by default — a tall group scrolls the section title off the top.** No alt-screen
  means the form prints into the normal terminal buffer; a long select (a `/models` list) makes the
  group taller than the window, so the terminal scrolls to the focused field and the `.Title()` you just
  added is gone above the fold. Fix: run the wizard on an alt-screen via `WithProgramOptions(...,
  tea.WithAltScreen())`. **`WithProgramOptions` *replaces* huh's defaults**, so re-supply
  `tea.WithOutput(os.Stderr)` (keeps stdout clean/pipeable) and `tea.WithReportFocus()` alongside it.
- **`Select.Filtering(true)` adds a focus-grabbing filter input.** It reads as a stray `/` box at the top
  of the list and (inline) the cursor-in-input pulls the scroll to it. Worth it for genuinely long
  lists; for a handful of options it's noise.
- **`Select.Height(n)` turns the options into a scrolling window.** Options past the window slide out of
  view with no scroll indicator — jarring. Don't reach for it to "tidy" a list; omit it so the list is
  static and the cursor moves. (Only worth it when a list is genuinely longer than the screen.)
- **Separate forms kill back-nav.** One form per question is the most common mistake. Merge into one
  multi-group form; split *only* across an I/O boundary (idiom #1), and gate answer-dependent sections
  with `WithHideFunc` (idiom #5) rather than a new form.
- **A confirm that's never checked writes anyway.** `Confirm.Value(&ok)` records the choice; it does not
  abort. Forget the `if !ok { return }` and the wizard writes the file whether the user said yes or no —
  and the happy path hides the bug. (Bit us in the first wizard cut.)
- **Static review summaries go stale.** `Note.Description(string)` snapshots before the form runs; after
  a back-up-and-edit it shows old values. Use `DescriptionFunc(fn, bindings)` (idiom #4).
- **`WithHideFunc` is on `Group`, not fields.** To hide one field conditionally, put it in its own group.
- **`go:embed` can't climb out of a package.** If the wizard needs embedded data (agents/skills), the
  `//go:embed` directive must live at the module root — paths are relative to the source file and `..`
  is disallowed. (See `embed.go` at the repo root + `internal/bundledir`.)
- **A multi-select's pre-selected options come from the slice you bind.** To default features *on*,
  pre-seed the `[]string` value with the option values, not just set a bool.
- **`Select` won't highlight a value missing from its options.** If you pre-fill from env and the
  value isn't in the discovered list (stale env), add it as an explicit option or the select silently
  won't match. (See `references/forms.md` → "tolerate stale pre-fills".)
- **Don't `fmt.Println` inside a running form.** It corrupts the alt-screen render. Print only after
  `Run()` returns.

## Validation Loop

After building or restyling a wizard:

1. `go build ./...` — catches the v1/v2 API mismatches (`.Title()` on Form, v2 import paths).
2. `gofmt -w` — huh's chained-builder style drifts under editing.
3. **Run it in a real terminal** — a form's feel (navigation, theme, placeholder visibility) can't be
   asserted in a unit test. Pipe-read a few answers; confirm ← returns and the confirm gate writes.
4. Unit-test the *output*, not the keystrokes: the function that turns answers into the emitted file
   (e.g. `cli.EmitServerConfig`) round-trips through the real config loader. The Huh form itself is
   not unit-tested by design.

## Resources

- `references/theme.md` — the copy-ready rubber-duck theme + the `runForm` wrapper. Read when theming a
  CLI surface.
- `references/forms.md` — the form patterns: the two-form (one I/O break) shape, `WithHideFunc` store
  groups, the live-summary review/confirm gate, the stale-prefill-tolerant select, and the v1 modifier
  table. Read when building or restructuring the wizard itself.

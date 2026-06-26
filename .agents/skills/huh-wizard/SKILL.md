---
name: huh-wizard
description: |
  How to build polished multi-stage interactive wizards with Charm `huh` (v1) — the form library quack's
  `quack init` / `quack server init` use. Covers the idioms that separate a generic form from a
  deliberate, navigable wizard: multi-group forms for built-in back-navigation, group titles/descriptions
  as section headers, placeholders + EchoModePassword + .Key() on fields, a final review/confirm group,
  and a custom theme. Includes the gotchas that bite (Form has no .Title() in v1 — use the first group;
  separate forms per question kill back-nav; embed paths can't climb out of a package). Use when adding
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

**Constraint:** a form's groups must all be known up front. If group N depends on the answer to
group N-1 (e.g. store questions gated on the features multi-select), you can't put them in one form —
run a *second* form after the first, accepting no back-nav across the boundary. Structure so the
dependency cut is between sections, not within one.

### 2. Group `.Title()` + `.Description()` = section headers

In v1, **`Form` has no `.Title()`** (that's v2). The section header is the **first group's** title +
description. A form with untitled groups reads as a flat list with no context.

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

### 4. End with a confirm group

A wizard should review before it writes. The `git` example ends with `huh.NewConfirm()`. A `Note` field
(`huh.NewNote().Title("Review").Description(summary)`) renders a non-interactive summary block, then a
confirm group gates the write. The user can back all the way out from the confirm.

```go
huh.NewForm(
  huh.NewGroup(huh.NewNote().Title("Review").Description(summary)),
  huh.NewGroup(huh.NewConfirm().Title("Write quack.yaml?").Value(&ok)),
)
```

### 5. Theme it (one helper, applied to every form)

Build the theme once (`huh.ThemeBase()` + accent overrides) and route every form through a `runForm`
helper so the look is consistent. See `references/theme-and-forms.md` for the full rubber-duck theme +
the `runForm` wrapper. Keep the default theme if there's no brand call — a custom theme is taste + risk
(ANSI in golden tests).

## Gotchas

- **`Form.Title()` does not exist in v1.** The examples on `charm.land/huh/v2` use `.Title()` on the
  form; our pinned `github.com/charmbracelet/huh v1.0.0` has it only on `Group`. Compile error:
  `*huh.Form has no field or method Title`. Fix: put the title on the first group.
- **Separate forms kill back-nav.** One form per question is the most common mistake. Merge into one
  multi-group form; only split where a later section depends on an earlier section's answer.
- **`go:embed` can't climb out of a package.** If the wizard needs embedded data (agents/skills), the
  `//go:embed` directive must live at the module root — paths are relative to the source file and `..`
  is disallowed. (See `embed.go` at the repo root + `internal/bundledir`.)
- **A multi-select's pre-selected options come from the slice you bind.** To default features *on*,
  pre-seed the `[]string` value with the option values, not just set a bool.
- **`Select` won't highlight a value missing from its options.** If you pre-fill from env and the
  value isn't in the discovered list (stale env), add it as an explicit option or the select silently
  won't match. (See `references/theme-and-forms.md` → "tolerate stale pre-fills".)
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

- Read `references/theme-and-forms.md` when implementing the theme or any of the form patterns above —
  it has the copy-ready rubber-duck theme, the `runForm` wrapper, the stale-prefill-tolerant select,
  and the full section-grouped `askProvider`/`askModels`/`askStores` shapes.

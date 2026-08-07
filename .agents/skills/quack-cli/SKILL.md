---
name: quack-cli
description: |
  Standards and decisions for building quack's one-binary `quack` CLI + terminal UI (M8). Covers the
  framework choice (Charm: Bubble Tea runtime + Bubbles components + Lipgloss styling + Huh for the
  `server init` wizard), cobra for the command tree (`chat`/`server`/`api` + `-p` print mode), the
  three-tier testing strategy (Update() table tests → teatest golden snapshots → httptest for TUI-free
  paths), and the hard rules that keep print mode / api pipeable and the models unit-testable. Use when
  adding to, reviewing, or debugging anything under `cmd/quack/`, `internal/tui/`, or `internal/cli/` -
  a new screen, a command, an SSE-driven view, the init wizard, or a TUI test. Do NOT use for the React
  chat UI (that's frontend-design), the SSE event vocabulary on the server, or openapi.yaml.
license: MIT
metadata:
  author: jason
  version: "1.0"
---

# Quack CLI & TUI

## Overview

How quack's terminal client is built and why. One multi-call `quack` binary (cobra command tree) talks to a `quack serve` process over **HTTP + SSE** - the same event vocabulary the React app renders. The interactive surface is a **Bubble Tea** program; the `server init` wizard is a **Huh** form; print mode (`quack -p`) and `quack api` are plain stdout writers with **no terminal UI at all**. This skill governs the *decisions* (which library, where code lives, how to test); concrete code skeletons live in `references/recipes.md`.

## When to Use

- Editing anything under `cmd/quack/`, `internal/tui/`, or `internal/cli/`.
- Adding a screen, a command/subcommand, an SSE-driven view, or the `server init` wizard.
- Writing or reviewing a TUI test, or diagnosing flaky golden-file snapshots.
- Deciding build-vs-reuse for a CLI ask (most of it is already a Charm primitive).

## When NOT to Use

- The **React chat UI** (`frontend/`) - use `frontend-design`. Same SSE vocabulary, different surface.
- The **server-side SSE event types**, REST handlers, or `openapi.yaml` - wrong layer.
- Agent `prompt.md` / behaviour - wrong layer.

## The stack (constraints)

**Charm ecosystem + cobra.** Pin these; do not reach for a second TUI lib for one widget.

| Concern | Library | Notes |
|---|---|---|
| TUI runtime | **`charmbracelet/bubbletea`** | Elm architecture: `Init/Update/View`. `Update` is a pure reducer. |
| Components | **`charmbracelet/bubbles`** | `list`, `textinput`, `textarea`, `spinner`, `viewport`. Don't hand-roll these. |
| Styling/layout | **`charmbracelet/lipgloss`** | CSS-for-terminal. Auto-detects color depth; **force-off when not a tty**. |
| Wizard forms | **`charmbracelet/huh`** | The `server init` wizard (select/confirm/input). Don't build a form loop by hand. |
| Command tree | **`spf13/cobra`** | 3-level tree (`quack chat node steer`) + `gh api`-style passthrough. |

**Rejected (and why):** tview / gocui (imperative window model, fights the streaming reducer; gocui unmaintained), termui (unmaintained 2019), gotui (young, dashboard-shaped, not chat), go-prompt (line-prompt only, no full screen). stdlib `flag` is the laziness floor but has no ergonomic nested subcommands - cobra earns its one dependency here.
Match the existing stack; do not add another.

## Where code lives

Mirror the frontend's split (logic separate from rendering, client separate from UI):

- **`cmd/quack/`** - `main` + cobra wiring only. Command funcs are **thin**: parse flags, dispatch into `internal/cli` or `internal/tui`. The old `cmd/server` folds in as the `quack serve` subcommand (one binary, one image - M10).
- **`internal/cli/`** - the **TUI-free** surface: the HTTP+SSE client, `-p` print mode, `quack api` passthrough, the `server init` wizard's config emit. **Imports no bubbletea** (keeps pipe/CI paths free of terminal deps).
- **`internal/tui/`** - Bubble Tea models (the `Update` reducers, ≈ `frontend/src/state/`). Imports the client from `internal/cli`; the client never imports back.

## Core principles

1. **`Update` is a pure reducer - side effects are `tea.Cmd`, never inline.** Network reads, timers, file writes happen inside returned `tea.Cmd` funcs, not in the body of `Update`. This is what makes the model unit-testable (`Update(msg) → (model, cmd)` with no I/O) and teatest deterministic. An `http.Get` in `Update` is the cardinal sin. → recipe in references.

2. **The SSE stream is a self-re-issuing command, one msg per event.** A `waitForEvent` `tea.Cmd` reads exactly one server event, wraps it as a `tea.Msg`, and `Update` re-issues `waitForEvent` to pull the next. Never loop-and-block inside `Update`. This is the terminal analog of `agentStream.ts`. → recipe.

3. **Reuse the OpenAPI types - don't redefine request/response structs.** The CLI is just another API client. Share the generated types / a generated client from `openapi.yaml` rather than hand-copying shapes; the SSE event union is the same one the React app parses. New endpoint → regenerate, don't retype.

4. **Print mode and `api` have no TUI.** `quack -p "<prompt>"` and `quack api` write plain text/JSON to stdout and **must not start a `tea.Program`** - they pipe into CI, `jq`, and shell. Gate ANSI/color on `isatty(stdout)` (Lipgloss honors `NO_COLOR`/non-tty, but verify); when piped, emit clean text.

5. **The `server init` wizard is Huh, and you test its output, not its keystrokes.** Huh owns the select/confirm/input loop (embedded / managed / external stores topology). Your code's job is turning the answers into a valid `quack.yaml` - test *that*, not the form interaction. → recipe.

6. **Slash-commands mirror cobra verbs.** In-TUI `/stop`, `/node stop`, `/queue`, `/approve`, `/answer` map to the same client calls the `quack chat …` verbs use. One implementation per action in `internal/cli`; both the command and the slash-command call it. No duplicated logic.

## CLI conventions (house style)

Follow the cross-tool consensus (clig.dev, POSIX utility conventions, Heroku/Atlassian/Microsoft).
The rulings that bind quack - most are already in the cobra wiring, so *preserve* them:

- **Flags label options; a positional arg names the subject.** Options are named flags (`--server`, `-p`); the *primary subject* (a chat id, a node id) is positional - `quack chat delete <id>`, `quack chat node steer <node> <msg>`. This is the `gh`/`docker` balance, **not** "flags for everything." Give every flag a short alias where one is idiomatic (`-p`).
- **Names: lowercase, kebab-case, consistent number.** `chat node steer`, never `chatNodeSteer` or `steer-chat`. Don't mix singular/plural across related flags (`--source` *and* `--sources`).
- **Streams compose: content → stdout, everything else → stderr.** Results/tokens/JSON to stdout so `| jq` and `> file` work; status, spinners, prompts, logs to stderr. (This is the print-mode gotcha, generalized.) Offer `--json` where a verb has structured output; `quack api` is always raw JSON.
- **Exit 0 = success, non-zero = error (POSIX).** `Execute()` error → `os.Exit(1)` already does this; keep `SilenceUsage: true` so a runtime failure isn't buried under a usage dump (a failed `RunE` is an error, not a misuse).
- **Errors say *what* + *how to fix*.** Not `request failed` - `cannot reach server at <url>: is` `quack serve` `running? pass --server or run` `quack server use`. The `notWired` stub is a placeholder; real handlers name the recovery.
- **Reaction for every action; suggest the next step.** Confirm success/failure plainly; after a terminal verb point at the obvious follow-up (`server init` → "now run `quack serve`"; `chat new` → print the chat id to `resume`).
- **Long work shows progress and is interruptible.** Spinner (Bubbles) for streaming/long calls; `^C` cancels cleanly - cancel the request *and* restore the terminal. Print mode shows a stderr status line, not a spinner.
- **`--help` is complete and discoverable** - cobra generates it from `Use`/`Short`, so write a real `Short` for **every** command (no blank leaves) and a one-glance `Long` on the root.

## Testing strategy (three tiers)

Mirror the frontend: logic tests cheap and many; render snapshots rare and integration-level.

| Tier | What | Tool | How many |
|---|---|---|---|
| **1 - reducer** | `Update(msg) → (model, cmd)`: state transitions, which `cmd` is returned | plain Go table tests, **no framework** | many - every meaningful transition |
| **2 - render** | the assembled program renders the SSE vocabulary correctly | **`charmbracelet/x/exp/teatest`** golden files | few - one or two per screen, not per-state |
| **3 - TUI-free** | print mode output, `api` passthrough, wizard→`quack.yaml`, the client | `net/http/httptest` stub server + table tests | per behaviour |

- **Tier 1 is the workhorse.** Because `Update` is pure (principle 1), feed it a msg and assert the new model + the returned `cmd`'s type. No PTY, no golden file, no flakes. This is `chatStore.test.ts`'s analog.
- **Tier 2 is integration glue, kept small.** teatest runs the model in a virtual terminal, captures the full frame, diffs against a `.golden`. Regenerate with `-update`. Use a **fixed terminal size** (`teatest.WithInitialTermSize(80, 24)`) or the golden is nondeterministic. One golden per screen is enough - don't snapshot every keystroke.
- **Tier 3 needs no terminal.** Point the client at an `httptest.Server` that replays a canned SSE stream; assert the printed/structured output. The wizard test asserts the emitted YAML round-trips through the real config loader.
- **E2E the compiled binary sparingly** (the "test it the way users run it" level): a handful of `os/exec` runs of the built `quack` asserting **exit code + stdout/stderr split** - `quack version` prints to stdout exit 0; an unknown command exits non-zero; `quack -p … | cat` is ANSI-free. Use Go's own `os/exec`, **not** a new harness - BATS/expect only if shell-level coverage is ever actually wanted (it isn't yet). `cmd/quack/main_test.go` covers the in-process slice (tree shape + `version` to stdout via `root.Execute()`); the exit-code and ANSI-free-pipe checks need the *built* binary.
- **Rejected:** microsoft/tui-test (TypeScript - wrong language for Go CI), tui-driver (framework-agnostic PTY assertions - only if we leave Bubble Tea, which we won't), **VHS** is for **docs GIFs**, not CI - optionally add `.tape` files for the README, never as a test gate.

> **Sources** (CLI design consensus, distilled into the rulings above): clig.dev · POSIX.1 §12 Utility Conventions · Heroku CLI Style Guide · Atlassian "10 principles for delightful CLIs" · Microsoft System.CommandLine design guidance. Testing levels: Smashing "Testing the CLI the way people use it".

## Gotchas

- **Experimental import path:** teatest is `github.com/charmbracelet/x/exp/teatest` (the `x` monorepo), not under `bubbletea`. Pin it; the API is still moving.
- **`Update` must not block or sleep.** A blocking read freezes the whole UI (single event loop). Always hand long work to a `tea.Cmd`. A test that hangs is usually a blocking `Update`.
- **Golden files are color-coded.** teatest captures ANSI. A Lipgloss style change rewrites every golden; regenerate with `-update` and eyeball the diff - don't blindly accept.
- **`internal/cli` must stay bubbletea-free.** If the print-mode / `api` path imports bubbletea (transitively too), piping breaks and CI builds drag in terminal deps. Keep the client in `internal/cli` and models in `internal/tui`; the dependency arrow points one way (tui → cli).
- **Don't `fmt.Println` inside a running `tea.Program`.** It corrupts the alt-screen render. Route output through the model's `View` (or `tea.Printf`/`tea.Println` for above-the-program lines).
- **Print mode color leak:** even with Lipgloss auto-detect, double-check `quack -p … | cat` emits no escape codes. CI logs and `jq` choke on ANSI. Honor `NO_COLOR`.
- **cobra command funcs are not the place for logic.** They parse and dispatch. Logic in `internal/cli`/`internal/tui` so it's testable without constructing a `*cobra.Command`.

## Validation Loop

After any change under `cmd/quack/`, `internal/cli/`, or `internal/tui/`:

1. `go test ./internal/tui/... ./internal/cli/... ./cmd/quack/...` - reducer + httptest tiers green.
2. teatest goldens: run once, then `go test -run TUI -update ./...` only if you intended a visual change; re-run without `-update` → clean.
3. `make vet && make fmt`. `CGO_ENABLED=0 go build ./cmd/quack` stays green (pure-Go - keeps M10 cross-compile trivial).
4. Behaviour, against a real `quack serve`: `quack -p "ping" | cat` → clean text, **no ANSI**; an interactive `quack chat new` streams a run and a slash-command (`/stop`) works; `quack server init` (embedded) emits a `quack.yaml` that `quack serve` then boots with **no containers**.

## Resources

- Read `references/recipes.md` when implementing any principle above - it has the copy-ready skeletons: the Bubble Tea `Model` (Init/Update/View), the self-re-issuing **SSE pump** `tea.Cmd`, a tier-1 `Update` table test, a tier-2 **teatest** golden test (with fixed term size + `-update`), the **Huh** `server init` wizard + its config-output test, the print-mode writer, and the cobra root + subcommand wiring. Load it when writing code, not when only choosing the approach.

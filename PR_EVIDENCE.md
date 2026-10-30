# PR evidence: ui-component-base

Branch `ui-component-base` is a local worktree branch (`worktree-agent-a41c527253dfbd293`), base `origin/main` 390ec7fc. Not pushed, not labeled. This file is untracked.

## Gates

All run from the final HEAD (86d45fd5 plus the untracked test files committed in it). Baseline on origin/main before any change was green: lint 0 errors / 41 warnings, knip clean, tsc clean, 793 tests.

| Command | Result |
| --- | --- |
| `npm ci` (frontend/) | exit 0 |
| `npm run lint` (eslint + check-stories + check-no-emoji) | exit 0; 0 errors, 37 warnings (baseline 41, all legacy no-floating-promises in tests); `check-stories: every component has a story`; `check-no-emoji: OK` |
| `npm run knip` | exit 0, no output |
| `npx tsc --noEmit` | exit 0, no output |
| `npm run build` (tsc + vite build) | exit 0; only the pre-existing >500 kB chunk warning |
| `npm test` | exit 0; 77 files, 792 tests passed |
| `npm run render-check` | exit 0; 1 file, 646 tests passed (215 s). The very first run after the new deps were installed failed with "Failed to import test file" (Vite re-optimising newly discovered Radix deps mid-run); rerun passed and every later run passed. render-check now scans `document.body` for stray comment text and covered dialogs, because Radix dialogs portal out of the story container; the 646 count did not change with that stricter scan. |
| `go build ./...` (repo root, embeds the freshly built SPA) | exit 0 |
| `go run ./tools/sloplint diff origin/main` | `sloplint: ok` |
| `go run ./tools/promptlint diff origin/main` | `promptlint: ok` (one pre-existing warn in the embedded dotagents snapshot) |

Not run: the `dupl` golangci-lint gate (Go only, no Go files touched), `scripts/coverage-diff.mjs` changed-line coverage gate (CI-only), the QA rig (AGENTS.md.local says runtime changes only; this is frontend). Not exercised by a human on a non-production server: AGENTS.md asks for a UI path to be checked on a non-prod server and named in the PR; I verified through Storybook and render-check only, so the PR author must add that line.

## Screenshots

`/tmp/claude-1000/-home-jason-workspace-quack/ee964ffb-bdc6-496b-b320-2349008e6a8e/scratchpad/ui-shots/`

`<page>-<width>-before.png` and `-after.png` for page in chat (Pages/Chat FinishedTurnWithArtifacts: finished plan card), artifact (Chat/ArtifactPanel WithResult), memory (Pages/Memory Default), plugins (Pages/Plugins Default) at width 1440 and 390. Taken from static Storybook builds (before = origin/main, after = HEAD) with Playwright against `iframe.html?id=...`. Extra: `*-after-dark.png` (dark theme; chat and plugins checked by eye, the other dark ones are unreviewed).

The visible change is deliberately small: flatter cards (no shadow), one 8dp control radius, a real 2px focus ring, calmer chip fill, token-driven page background. Screens are not restyled in this PR.

## Verification gaps

- Phone-width menus are now Radix popovers, not bottom sheets (the old Sheet turned menus into a bottom sheet below 600px). Items are still 44px tall. Not checked on a real phone.
- Focus restore after closing Dialog/Sheet is covered by a jsdom test (`ui/dialog.rtl.test.tsx`), not a real browser.
- Contrast was computed (see index.css token comments and the pairs checked: muted-foreground 5.0:1 on page bg and 4.67:1 on muted in light, 5.0:1 on muted in dark, input border 3.15:1 light / 3.48:1 dark on card) but only for the token pairs, not every raw-gray text in unmigrated screens.
- MemoryEntry kebab and DagNode menu: focus return after a menu item opens a dialog is Radix default, no test.

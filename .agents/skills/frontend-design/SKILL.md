---
name: frontend-design
description: |
  Design decisions and gotchas for the quack chat frontend (React 19 + Vite + Tailwind v4 +
  react-markdown, shadcn-style components on Radix in components/ui). Covers: inventory-before-building,
  re-render isolation for streaming chat instead of virtualization, optimistic-first store writes,
  the components/ui primitives and when to add one, semantic tokens in `@theme`, markdown code-block
  highlight + copy, accessibility, stories-vs-tests placement, and mobile-first responsive design
  (Material 3 breakpoints, off-canvas nav, 44px touch targets, dialogs-as-sheets on compact width).
  Use when restyling/extending the chat UI under frontend/src (pages/Chat.tsx, components/, state/),
  fixing typing lag or render jank, fixing a mobile/responsive/small-viewport layout defect, adding a
  chat feature, or deciding build-vs-reuse for a UI request.
  Do NOT use for backend/API-contract changes, openapi.yaml, agent prompts, or non-frontend work.
license: MIT
---

# Frontend design (quack chat UI)

## Overview

How to make changes to quack's chat frontend that fit its grain: lean stack, streaming-first, re-render-isolated. This skill is **decision-level guidance** - when to reach for which pattern and the gotchas that bite. Concrete file-level recipes and code live in `references/recipes.md`. The skill stops at the frontend: it does not cover the SSE event vocabulary on the server, the API contract (`openapi.yaml`), or agent behaviour.

## When to use

- Restyling the chat UI or its tone ("the UI looks too bright / dated").
- Fixing **typing lag**, render jank, or "no loading indicator until the first token".
- Adding a chat-surface feature (search, copy buttons, code highlighting, auto-resize input).
- Deciding **build vs reuse** for a UI ask - most "agent frontend best practices" already exist here.
- Editing `frontend/src/pages/Chat.tsx`, `frontend/src/components/*`, or `frontend/src/state/*`.

## When NOT to use

- The change is in the **backend, SSE event types, `openapi.yaml`, or an agent `prompt.md`** - wrong layer.
- It needs **deterministic execution / typed I/O / external access** (a new endpoint, a tool) - use a tool, not UI patterns.
- It's a non-chat page or a generic React question with no quack-specific angle - answer directly.

## The stack (constraints)

**React 19 + Vite + Tailwind CSS 4 + Radix primitives + TanStack Query + react-markdown.** UI primitives follow shadcn/ui conventions: the component source is copied into `frontend/src/components/ui/` (not installed as a library), built on `@radix-ui/react-*` packages, `class-variance-authority` for variants, and `cn()` (`lib/utils.ts`, clsx + tailwind-merge) for class merging. There is no virtualization lib and no Zustand/Jotai. State is in `frontend/src/state/`: `chatStore.ts` is a plain `Map`-backed store with a `subscribe` seam; `agentStream.ts` parses the SSE event vocabulary. **Match this** - do not add a state library for a single widget, and reach for the laziest rung that holds (native CSS, then an existing primitive, then a few lines) before a new dependency.

### The primitives (`components/ui/`)

Button (variants default/secondary/outline/ghost/link/destructive; sizes default/sm/icon/inline), Badge (the one chip shape, `tone` per hue; replaced the old CHIP constant), Card (`default` bordered surface, `flat` nested well), Input/Textarea, Dialog (bottom sheet below `medium`, centred above; `placement="blocking"` for the version wall), Sheet (left edge drawer), DropdownMenu, Popover, Tooltip, Separator. Rules:

- **Use a primitive before hand-rolling.** A new `<button>`, chip, card surface, field, menu, popover or dialog goes through these; do not restyle them per screen with raw `gray-*` pairs.
- **Add a primitive only when two call sites need it and Radix has the behaviour** (focus trap, roving focus, collision handling). Copy the shadcn component, adapt it to the tokens below, export only what is used (knip fails on unused exports), and add `<name>.stories.tsx` (check-stories enforces one per file).
- **Pin Radix versions exactly** in `package.json`; add only the `@radix-ui/react-*` package the new primitive wraps.
- **Never leave a native `<dialog>`/popover-API element.** Radix content portals into `<body>` at z-50 (dialogs, sheets), z-60 (menus, popovers, tooltips) and z-70 (the blocking wall); a native top-layer dialog would cover all of them. Dialogs get an accessible name through `DialogContent`'s `title` prop (rendered sr-only) and keep their own visible headings.
- **Radix does not return focus without a Trigger.** `DialogContent`/`SheetContent` record and restore the opener themselves (`useOpenerFocus`); keep that when changing them.
- **`cn()` knows `text-2xs`.** tailwind-merge reads an unknown `text-*` as a colour and drops the real one, so custom font sizes are registered in `lib/utils.ts`.
- **Tests:** Radix menus open on `pointerdown` (userEvent, or a dispatched `pointerdown` with `button: 0`), content renders in a portal (query `document.body`/`screen`, not the render container), overlays carry `data-slot="overlay"`, and `vitest.setup.ts` stubs ResizeObserver/pointer-capture for jsdom.

## Core principles

0. **Inventory before building.** Streaming, an always-visible Stop button, markdown+GFM, sticky input, Enter/Shift+Enter, mobile sidebar, dark mode, per-response model names, and the DAG view already exist - including the off-canvas drawer mechanics a nav fix would otherwise reinvent: the navigation drawer `NavRail` (the app's only navigation shape since #1171: closed it renders nothing, open it floats over the content with a backdrop, and it's opened from the ⊞ NavToggle in each page's header) and the chat-list drawer (the same off-canvas pattern). Grep first; ship the gap, not a rebuild.

1. **Typing lag is a re-render problem, not a DOM-count problem - isolate, don't virtualize.** Move draft state into its own component so keystrokes don't re-render the turn list; memoize completed turns so they don't re-parse markdown while a later turn streams; key any sibling-derived `useMemo` to a ref that's stable during streaming. Reach for `react-virtuoso` only if node count alone still lags afterward. → recipe in references.

2. **Optimistic-first store writes.** In `chatStore.submit`, write the optimistic state (live turn, or a `submitting` flag) **before** any awaited fetch, never after - otherwise the spinner waits on a round-trip. Keep previously-rendered content visible across the archive fetch so nothing blinks out. → recipe in references.

3. **Theming lives in `index.css`.** Two layers: the `@theme` gray/blue scale overrides retone every raw `gray-…`/`blue-…` utility at once (screens not yet migrated), and the semantic tokens (`--background`, `--foreground`, `--card`, `--popover`, `--muted`, `--muted-foreground`, `--accent`, `--primary`, `--destructive`, `--border`, `--input`, `--ring`, `--scrim`, `--elevation-overlay`) on `:root`/`.dark`, exposed to Tailwind through `@theme inline` as `bg-card`, `text-muted-foreground`, `border-border`, `ring-ring`, `shadow-overlay`. New and migrated code uses the semantic names, which flip with `.dark` so no `dark:` pair is needed. Shape scale: `rounded-sm` (4) badges, `rounded-md` (8) controls, `rounded-xl` (12) surfaces and overlays. Elevation: surfaces are flat with a hairline border; only floating layers (menus, popovers, dialogs, the composer) take `shadow-overlay`. Universal rules regardless: **no pure-black dark background** (tinted near-black); **desaturate accents ~10–20% and use off-white, not pure white, text in dark mode**; **verify WCAG AA** (4.5:1 body, 3:1 large/UI) - don't eyeball muted greys. → recipe + contrast-check script in references.

4. **Markdown code blocks: work *with* the rehype pipeline.** Highlighting and the copy button go through `react-markdown` plugin ordering + a `components` override, not a parallel renderer.

5. **Native platform features first.** CSS auto-grow (`field-sizing-content`) over a resize hook; a plain `Array.filter` over already-loaded data over a search backend.

6. **Accessibility is not optional.** `role="log"`/`aria-live` on the streamed region, `role="status"` on spinner bubbles, `aria-label` on every icon-only button (text buttons need none).

7. **Mobile-first.** Design compact (<600px) first, enhance upward with `sm:`/`md:` - never shrink a desktop layout down. Three non-negotiables: no horizontal page scroll, 44px touch targets on repeatedly-tapped controls, and drawers/sheets (not narrowed-but-persistent chrome) for nav and dialogs on compact width. → decision rules, sourced principles, and the #1131 audit's before/after fixes in `references/mobile-first.md`.

8. **Stories vs tests.** Add a co-located `*.stories.tsx` for newly extracted/changed surfaces. Tests run **node-env, logic-only** (no `@testing-library/react`) - write store/reducer tests; do **not** stand up jsdom + a render lib to assert a one-line filter or a clipboard call.

## Gotchas

- **`*/` inside a CSS comment** (e.g. writing `bg-gray-*/border-*`) closes the comment early and leaks stray CSS → a build warning. Spell globs out in prose.
- **rehype plugin order:** `rehype-highlight` must run **after** `rehype-sanitize`, or its `hljs` classes get stripped. The `language-*` class survives sanitize, so language detection still works.
- **`useMemo` key for per-turn props:** key on `[state.turns, live?.userText]`. `state.turns` keeps a stable ref while only `live` changes during streaming (store handlers spread `{ ...s, live }`), so it won't recompute per token. Keying on the wrong thing silently reintroduces the lag.
- **Pass `isCopied` (a boolean), not the whole `copied` state**, to memoized turns - else every turn re-renders on any copy.
- **`field-sizing-content`** is Chromium-only; it degrades to a fixed scrolling box elsewhere - fine for a self-hosted tool, but note it.
- **Muted greys silently fail WCAG AA.** A warm/light `gray-500` for secondary text can land at ~4.1:1 on the page background - under the 4.5:1 body threshold. Run the contrast script (in references) on the actual pairings after any `@theme` change; nudge the shade darker rather than guessing. `gray-400`-level decorative metadata (timestamps) at ~3:1 is acceptable.
- **Status/state never by colour alone** (WCAG 1.4.1, ~8% of men have red-green CVD): pair every state colour with an icon, label, or motion. `role="status"` spinners already satisfy this.
- **Code blocks carry their own theme** (a `highlight.js` CSS theme), independent of bubble colours - pick one that reads in both light and dark (an always-dark code theme is a fine, consistent choice).

## Validation loop

Do the change → run the CI gate → fix → repeat until clean:

```bash
cd frontend && npx tsc --noEmit && npx eslint src/ && npm test && npm run build
```

Then confirm behaviour in `npm run dev` (and `npm run storybook` for new stories): typing stays smooth in a long chat, the spinner shows instantly on a follow-up message, code blocks highlight and copy. Watch the `npm run build` output for CSS warnings (see the `*/`-in-comment gotcha).

Finish with `npm run render-check` (#1192) - it mounts every story in a real Chromium at mobile/desktop x light/dark and catches what the commands above cannot: a stray JSX text node (a `//` comment left where `{/* */}` was meant), a dialog stacked under a sibling, and horizontal overflow. See `docs/frontend-testing.md`.

## Resources

- `references/recipes.md` - concrete file-level recipes and code (the Composer/TurnView/ChatList extraction, the `useMemo`/`memo` wiring, the optimistic-write reorder in `chatStore.submit`, the `@theme` override, the rehype-highlight + `CopyablePre` setup). **Read it when implementing** any of the principles above, not when only deciding the approach.
- `references/mobile-first.md` - sourced mobile-first decision rules (Material 3 breakpoints, NN/g nav research, WCAG 2.2 target size, MDN/Tailwind v4 responsive design), quack-specific mappings, the #1131 audit's before/after fixes, and the emulate-and-verify validation loop. **Read it when** fixing a mobile/responsive layout defect, adding a compact-width variant to nav/dialogs/cards, or auditing touch-target sizes.

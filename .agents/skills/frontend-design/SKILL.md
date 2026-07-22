---
name: frontend-design
description: |
  Design decisions and gotchas for the quack chat frontend (React 19 + Vite + Tailwind v4 +
  react-markdown, no component library). Covers: inventory-before-building, re-render isolation
  for streaming chat (extract input/list, memoize turns) instead of virtualization, optimistic-first
  store writes so loading indicators show instantly, global theming via Tailwind v4 `@theme` scale
  overrides, markdown code-block highlight + copy through the rehype pipeline, native-CSS-first inputs,
  accessibility for streamed content, and stories-vs-tests placement.
  Use when restyling or extending the chat UI under frontend/src (pages/Chat.tsx, components/, state/),
  fixing typing lag or render jank, adding a chat feature, or deciding build-vs-reuse for a UI request.
  Do NOT use for backend/API-contract changes, openapi.yaml, agent prompts, or non-frontend work.
license: MIT
---

# Frontend design (quack chat UI)

## Overview

How to make changes to quack's chat frontend that fit its grain: lean stack, streaming-first, re-render-isolated. This skill is **decision-level guidance** — when to reach for which pattern and the gotchas that bite. Concrete file-level recipes and code live in `references/recipes.md`. The skill stops at the frontend: it does not cover the SSE event vocabulary on the server, the API contract (`openapi.yaml`), or agent behaviour.

## When to use

- Restyling the chat UI or its tone ("the UI looks too bright / dated").
- Fixing **typing lag**, render jank, or "no loading indicator until the first token".
- Adding a chat-surface feature (search, copy buttons, code highlighting, auto-resize input).
- Deciding **build vs reuse** for a UI ask — most "agent frontend best practices" already exist here.
- Editing `frontend/src/pages/Chat.tsx`, `frontend/src/components/*`, or `frontend/src/state/*`.

## When NOT to use

- The change is in the **backend, SSE event types, `openapi.yaml`, or an agent `prompt.md`** — wrong layer.
- It needs **deterministic execution / typed I/O / external access** (a new endpoint, a tool) — use a tool, not UI patterns.
- It's a non-chat page or a generic React question with no quack-specific angle — answer directly.

## The stack (constraints)

**React 19 + Vite + Tailwind CSS 4 + TanStack Query + react-markdown.** All components are custom Tailwind — **no shadcn / Radix / Mantine**, no virtualization lib, no Zustand/Jotai. State is in `frontend/src/state/`: `chatStore.ts` is a plain `Map`-backed store with a `subscribe` seam; `agentStream.ts` parses the SSE event vocabulary. **Match this** — do not add a component or state library for a single widget. Reach for the laziest rung that holds (native CSS → existing dep → a few lines) before a new dependency.

## Core principles

0. **Inventory before building.** Streaming, an always-visible Stop button, markdown+GFM, sticky input, Enter/Shift+Enter, mobile sidebar, dark mode, per-response model names, and the DAG view already exist. Grep first; ship the gap, not a rebuild.

1. **Typing lag is a re-render problem, not a DOM-count problem — isolate, don't virtualize.** Move draft state into its own component so keystrokes don't re-render the turn list; memoize completed turns so they don't re-parse markdown while a later turn streams; key any sibling-derived `useMemo` to a ref that's stable during streaming. Reach for `react-virtuoso` only if node count alone still lags afterward. → recipe in references.

2. **Optimistic-first store writes.** In `chatStore.submit`, write the optimistic state (live turn, or a `submitting` flag) **before** any awaited fetch, never after — otherwise the spinner waits on a round-trip. Keep previously-rendered content visible across the archive fetch so nothing blinks out. → recipe in references.

3. **Global theming via Tailwind v4 `@theme`.** Override the built-in color scale once in `index.css` to recolor every existing utility — no per-component edits. This is the lazy lever for an *existing* app already using raw `gray-…`/`blue-…` classes; semantic tokens (`--color-bg-default`…) are the textbook answer but require rewriting every component, so only reach for them greenfield or when divergence grows. Universal rules regardless: **no pure-black dark background** (tinted near-black); **desaturate accents ~10–20% and use off-white, not pure white, text in dark mode**; **verify WCAG AA** (4.5:1 body, 3:1 large/UI) — don't eyeball muted greys. → recipe + contrast-check script in references.

4. **Markdown code blocks: work *with* the rehype pipeline.** Highlighting and the copy button go through `react-markdown` plugin ordering + a `components` override, not a parallel renderer.

5. **Native platform features first.** CSS auto-grow (`field-sizing-content`) over a resize hook; a plain `Array.filter` over already-loaded data over a search backend.

6. **Accessibility is not optional.** `role="log"`/`aria-live` on the streamed region, `role="status"` on spinner bubbles, `aria-label` on every icon-only button (text buttons need none).

7. **Stories vs tests.** Add a co-located `*.stories.tsx` for newly extracted/changed surfaces. Tests run **node-env, logic-only** (no `@testing-library/react`) — write store/reducer tests; do **not** stand up jsdom + a render lib to assert a one-line filter or a clipboard call.

## Gotchas

- **`*/` inside a CSS comment** (e.g. writing `bg-gray-*/border-*`) closes the comment early and leaks stray CSS → a build warning. Spell globs out in prose.
- **rehype plugin order:** `rehype-highlight` must run **after** `rehype-sanitize`, or its `hljs` classes get stripped. The `language-*` class survives sanitize, so language detection still works.
- **`useMemo` key for per-turn props:** key on `[state.turns, live?.userText]`. `state.turns` keeps a stable ref while only `live` changes during streaming (store handlers spread `{ ...s, live }`), so it won't recompute per token. Keying on the wrong thing silently reintroduces the lag.
- **Pass `isCopied` (a boolean), not the whole `copied` state**, to memoized turns — else every turn re-renders on any copy.
- **`field-sizing-content`** is Chromium-only; it degrades to a fixed scrolling box elsewhere — fine for a self-hosted tool, but note it.
- **Muted greys silently fail WCAG AA.** A warm/light `gray-500` for secondary text can land at ~4.1:1 on the page background — under the 4.5:1 body threshold. Run the contrast script (in references) on the actual pairings after any `@theme` change; nudge the shade darker rather than guessing. `gray-400`-level decorative metadata (timestamps) at ~3:1 is acceptable.
- **Status/state never by colour alone** (WCAG 1.4.1, ~8% of men have red-green CVD): pair every state colour with an icon, label, or motion. `role="status"` spinners already satisfy this.
- **Code blocks carry their own theme** (a `highlight.js` CSS theme), independent of bubble colours — pick one that reads in both light and dark (an always-dark code theme is a fine, consistent choice).

## Validation loop

Do the change → run the CI gate → fix → repeat until clean:

```bash
cd frontend && npx tsc --noEmit && npx eslint src/ && npm test && npm run build
```

Then confirm behaviour in `npm run dev` (and `npm run storybook` for new stories): typing stays smooth in a long chat, the spinner shows instantly on a follow-up message, code blocks highlight and copy. Watch the `npm run build` output for CSS warnings (see the `*/`-in-comment gotcha).

## Resources

- `references/recipes.md` — concrete file-level recipes and code (the Composer/TurnView/ChatList extraction, the `useMemo`/`memo` wiring, the optimistic-write reorder in `chatStore.submit`, the `@theme` override, the rehype-highlight + `CopyablePre` setup). **Read it when implementing** any of the principles above, not when only deciding the approach.

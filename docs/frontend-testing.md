# Frontend testing

The frontend gate is `tsc --noEmit`, `eslint`, `vitest run`, `build`, `knip`, `check-stories` (#1192, part 1) and `render-check` (#1192, part 2), run in that order in CI's `frontend-build` and `render-check` jobs.

## render-check

`npm run render-check` (in `frontend/`) mounts every `*.stories.tsx` export via Storybook's portable-stories API (`composeStories` from `@storybook/react-vite`) inside a real Chromium session (Vitest browser mode, Playwright provider - `vitest.render-check.config.ts`), at two viewports (390x844 mobile, 1280x800 desktop) and two themes (light/dark, toggled the same way `.storybook/preview.tsx` does).

It exists because #1192 shipped two PRs that passed every other gate check yet were unusable in a real browser:

- **#1189**: a nav drawer overlay stacked *under* a sibling at 1280px - only visible by checking which element is actually the top hit at its own screen position.
- **#1191**: a bare `//` comment inside JSX children rendered as a visible text node above the composer - invisible to a query-by-role/label RTL test.

For each story x viewport x theme combination, `render-check` asserts:

1. **No stray comment-shaped text node.** Walks every text node (`TreeWalker`) outside `<pre>`/`<code>`/font-mono containers (which legitimately render `//`-prefixed data - grep matches, diffs) and fails if one starts with `//` or `/*`.
2. **No covered dialog.** Every `[role="dialog"]`/`[open]` element must be the actual top hit (`document.elementFromPoint`) at its own center - catches the #1189 stacking-context class of bug.
3. **No horizontal overflow.** Each story renders inside a fixed-size frame matching its viewport; the frame's `scrollWidth` must not exceed its `clientWidth`.
4. **No `console.error` during render.**

A screenshot is captured per combination into `frontend/render-check/` (gitignored - not checked in). To inspect a failure locally, run `npm run render-check` and open the PNG matching the failing test name printed in the output.

Stories that render at a fixed desktop width (most existing ones - no mobile intent) skip the mobile-viewport pass; a story that does care about phone width opts in by including "mobile" in its export name (matching the existing convention: `Composer.stories.tsx`'s `MobileViewport360`, `ArtifactPanel.stories.tsx`'s `WithJudgeNotesMobile`).

## check-stories

`npm run check-stories` (`frontend/scripts/check-stories.mjs`) fails when a `.tsx` file under `src/components/` or `src/pages/` exports a capitalized function component with no matching `.stories.tsx` sibling - render-check can only catch a defect in a component that has a story at all.

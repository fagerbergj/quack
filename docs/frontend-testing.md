# Frontend testing

CI runs three separate jobs for `frontend/`: `frontend-build` (`tsc --noEmit` → `eslint` → `knip` → `build`), `frontend-test` (`vitest run`), and `render-check` (#1192, part 2 - this doc). `check-stories` (#1192, part 1, [#1207](https://github.com/fagerbergj/quack/pull/1207)) is not merged yet - once it lands it adds a `check-stories` step to `frontend-build`, right after `eslint`.

## render-check

`npm run render-check` (in `frontend/`) mounts every `*.stories.tsx` export via Storybook's portable-stories API (`composeStories`/`setProjectAnnotations` from `@storybook/react-vite`) inside a real Chromium session (Vitest browser mode, Playwright provider - `vitest.render-check.config.ts`), at two viewports (390x844 mobile, 1280x800 desktop) and two themes. The real `.storybook/preview.tsx` module (its `withTheme` decorator and `index.css` import) is applied via `setProjectAnnotations`, so the checks below run against the app's actual Tailwind rules and dark-mode toggling, not a hand-rolled stand-in.

It exists because #1192 shipped two PRs that passed every other gate check yet were unusable in a real browser:

- **#1189**: a nav drawer overlay stacked *under* a sibling at 1280px - only visible by checking which element is actually the top hit at its own screen position.
- **#1191**: a bare `//` comment inside JSX children rendered as a visible text node above the composer - invisible to a query-by-role/label RTL test.

For each story x viewport x theme combination, `render-check` asserts:

1. **No stray comment-shaped text node.** Walks every text node (`TreeWalker`) outside `<pre>`/`<code>`/font-mono containers (which legitimately render `//`-prefixed data - grep matches, diffs) and fails if one starts with `//` or `/*`.
2. **No covered dialog.** Every `[role="dialog"]`/`[open]` element must be the actual top hit (`document.elementFromPoint`) at its own center - catches the #1189 stacking-context class of bug.
3. **No horizontal overflow.** Each story renders inside a fixed-size frame matching its viewport; the frame's `scrollWidth` must not exceed its `clientWidth`.
4. **No `console.error` during render.**

A screenshot is captured per combination into `frontend/render-check/` (gitignored - not checked in). To inspect a failure locally, run `npm run render-check` and open the PNG matching the failing test name printed in the output.

Stories that render at a fixed desktop width (most existing ones - no mobile intent) skip the mobile-viewport pass. A story opts in to the mobile pass explicitly via `parameters.renderCheck.viewports: ['mobile']` (or `parameters.viewport.defaultViewport` naming a mobile size); a story with neither falls back to matching "mobile" in its export name (the existing convention: `Composer.stories.tsx`'s `MobileViewport360`, `ArtifactPanel.stories.tsx`'s `WithJudgeNotesMobile`).

## check-stories

`npm run check-stories` (`frontend/scripts/check-stories.mjs`) fails when a `.tsx` file under `src/components/` or `src/pages/` exports a capitalized function component with no matching `.stories.tsx` sibling - render-check can only catch a defect in a component that has a story at all.

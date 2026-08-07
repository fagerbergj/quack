---
name: react-testing
description: |
  React Testing Library + Vitest/Jest best practices (2025–2026): unit, integration, mocking,
  async, CI/CD, and advanced (visual-regression, a11y, performance) testing. Covers the Testing
  Trophy philosophy, query priority (getByRole first, getByTestId last), userEvent over fireEvent,
  findBy/waitFor async patterns, custom render with providers, vi.fn/spyOn/mock, renderHook, React
  Query + Suspense testing, Vitest vs Jest config (pools, sharding, coverage thresholds), and when
  to reach for axe-core, Chromatic/Playwright CT, or the Profiler.
  Use when writing or reviewing React component/hook tests, picking a query or interaction API,
  setting up Vitest/Jest config or coverage gates, or deciding on visual/a11y/perf testing.
  Do NOT use for non-React testing, Go backend tests, or E2E flow design unrelated to components.
license: MIT
---

# React testing (RTL + Vitest/Jest)

## Overview

Decision-level guidance for testing React the way users actually use it.
The guiding rule: **the more your tests resemble how the software is used, the more confidence they give** ([Kent C. Dodds](https://kentcdodds.com/blog/write-tests)).
Invest most in integration-level tests; test behaviour, not internals.

Code templates and the full tables live in `references/recipes.md`.
Config, parallelism, and coverage live in `references/ci-and-tooling.md`.
Visual/a11y/perf live in `references/advanced.md`.

quack's frontend already uses **Vitest + RTL + MSW** - match that grain before adding deps.

## When to use

- Writing or reviewing a component, hook, form, or async-fetch test.
- Picking a query (`getByRole` vs `getByTestId`) or interaction API (`userEvent` vs `fireEvent`).
- Setting up / tuning Vitest or Jest config, parallelism, coverage thresholds, or CI gates.
- Deciding whether to add visual-regression, accessibility, or performance testing.

## When NOT to use

- Non-React code, Go backend tests, or pure E2E user-journey design.
- A question already answered by the repo's existing test setup - read those first.

## Core rules (the reflexes)

1. **Never test internal state or instance methods.** Test what the user sees.
2. **Query by accessibility, not implementation.** Role/label/text - never CSS class or component name.
3. **Don't wrap `render()` in `act()`** - RTL already does. Don't keep a `wrapper` var; destructure `{ rerender }`.
4. **Use the global `screen`** for queries (pre-bound to `document.body`).
5. **Default to `userEvent`, not `fireEvent`** - it models the real browser event chain and is async (`await` it).
6. **Async = `findBy*` / `waitFor` / `waitForElementToBeRemoved`.** Never `setTimeout` or fake timers for async UI.

## Query priority (memorize this order)

`getByRole` → `getByLabelText` → `getByPlaceholderText` → `getByText` → `getByDisplayValue` → `getByAltText` → `getByTestId` (last resort).
Variant by intent:

- **`getBy*`** - element you expect present (throws if missing = useful signal).
- **`queryBy*`** - asserting absence (returns `null`).
- **`findBy*`** - element appearing after async work (polls via `waitFor`, ~1000ms default).

## Mocking, at a glance

- `vi.fn()` - a fake function (assert calls/args). `vi.spyOn(obj, 'm')` - observe/replace one method.
- `vi.mock('mod', factory)` - replace a whole module (API client, custom hook). Jest mirrors all three.
- ESM mock hoisting gotcha → `vi.hoisted()`. Network-level mocking → MSW, not per-call stubs.
- Hooks → `renderHook` (built into `@testing-library/react` on React 18+); pass `wrapper` for providers.

## Tooling decisions

- **New Vite/ESM project → Vitest** (native ESM, shares `vite.config.ts`, faster watch). **React Native / large legacy Jest → stay on Jest.** See `references/ci-and-tooling.md`.
- **Coverage gate**: `thresholds` in config fails CI on drop. Industry baseline ~80% statements/ functions/lines, ~75% branches. Report `lcov` to Codecov/Coveralls.
- **Snapshots: avoid** for app components (rubber-stamped, break on irrelevant changes, test structure not behaviour). OK only for truly static UI-library primitives.

## Advanced (reach for these deliberately - `references/advanced.md`)

- **Visual regression**: Chromatic (Storybook-native, component-level) → first choice if Storybook exists; Playwright CT (real browser, free, manual baselines); Percy/Applitools (page-level).
- **Accessibility**: `jest-axe` inside the RTL suite; run `axe()` *after* interactions, not just on mount. Catches missing labels/contrast/ARIA - not whether alt text is *meaningful*.
- **Performance**: React `<Profiler>` API; `vitest-react-profiler` for render-count assertions; performance budgets (unit render-time → page TTI → Lighthouse CI) in the pipeline.

# Advanced: visual regression, accessibility, performance, Storybook

## Snapshot testing - avoid for app components

- **Pros**: one assertion, catches unexpected change on refactors/dep bumps, fine for stable UI-library primitives.
- **Cons**: rubber-stamped "accept" negates value; large snapshots un-reviewable in PRs; break on reordered attrs / class changes / React internal wrappers with zero functional change; **validates structure, not behaviour** - logic bugs pass.
- **Verdict (2025–2026)**: prefer explicit behavioural assertions (RTL queries + userEvent). Snapshots only for truly static primitives.

## Visual regression

| Tool | Scope | Integration | Price |
|---|---|---|---|
| Chromatic | component | Storybook-native | ~$199/mo |
| Percy | page | any URL/crawler | ~$589/mo |
| Applitools | both (AI) | SDK | ~$500/mo |
| BackstopJS / Playwright CT | screenshot | open-source | free |

- **Chromatic** - every Storybook story becomes a visual test. First choice with mature Storybook.
  ```ts
  export const Primary = {
    args: { variant: 'primary', label: 'Submit' },
    parameters: { chromatic: { modes: {
      mobile: { viewport: { width: 375 } }, desktop: { viewport: { width: 1280 } }, dark: { theme: 'dark' },
    } } },
  };
  ```
- **Playwright CT** - renders components in a real browser, same locator/assertion model as E2E; free, manual baselines.
  ```ts
  import { test, expect } from '@playwright/experimental-ct-react';
  test('renders correctly', async ({ mount }) => {
    const c = await mount(<MyComponent title="Hello" />);
    await expect(c).toHaveScreenshot();
  });
  ```
- **Percy / Applitools** - page-level full-app regression.

## Accessibility (axe-core in the RTL suite)

```tsx
import { axe, toHaveNoViolations } from 'jest-axe';
expect.extend(toHaveNoViolations);

it('passes a11y after interaction', async () => {
  render(<LoginForm />);
  await userEvent.type(screen.getByLabelText(/email/i), 'user@example.com');
  expect(await axe(document.body)).toHaveNoViolations();
});
```

- **Catches**: missing alt/labels, contrast, invalid ARIA, heading structure, keyboard - 80+ rules.
- **Doesn't catch**: whether alt text is *meaningful*, logical focus order, complex interactions.
- Run **after interactions** - a11y is a property of every state. Keep it inside the RTL suite, not a separate step.

## Performance

```tsx
<Profiler id="App" onRender={(id, phase, actualDuration) =>
  console.log(`${id}: ${phase} - ${actualDuration}ms`)}>
  <App />
</Profiler>
```

- **Vitest**: `vitest-react-profiler` - count renders, detect unnecessary re-renders, mount/update phases, profile hooks.
- **Jest/prod flows**: `react-automation-profiler` (Profiler + Puppeteer) for cross-build charts.
- **Budgets in CI**: unit render-time (<16ms/component) → integration TTI/bundle size (<3s on 3G via Puppeteer/Playwright) → E2E Lighthouse CI for Core Web Vitals.

## Storybook + testing

- **Storybook 9+ Vitest addon** is the modern standard: `npx storybook add @storybook/addon-vitest`. Adds visual + a11y tests and coverage from stories.
- **Interaction tests** via `play()` are the single source of truth - auto-converted to Vitest component tests:
  ```ts
  export const Clickable = {
    args: { label: 'Click me' },
    play: async ({ canvasElement }) => {
      const canvas = within(canvasElement);
      await userEvent.click(canvas.getByRole('button', { name: /click me/i }));
    },
  };
  ```
- **Legacy (Storybook 7/8)**: Jest-based Test Runner (Playwright under the hood) also runs `play()`.

## Flaky async tests

Rarely random - deterministic, rooted in implicit shared state.
Fix with proper async waits (`findBy*`/`waitFor`), provider/module isolation, and avoiding leaked singletons across files (see `pool`/`isolate` in `ci-and-tooling.md`).
Never `setTimeout`/fake timers for async UI.

# v1 QA rig

The "exercise it on a non-production server" check, made repeatable. `v1/tools/qa/` starts a throwaway distributor on free ports, drives the web UI with Playwright at desktop and phone widths in light and dark, and prints a pass/fail line per step with the screenshot paths. A v1 pull request that changes the UI or a page route cites a scenario run in its description (the scenarios run, the summary, and which screenshots you looked at).

## Run

From `v1/`:

```
npm ci
npx playwright install chromium      # or: export QA_CHROMIUM=/path/to/chrome
node tools/qa/run.ts --out /tmp/qa-out
```

`run.ts` builds `packages/web` once, then for each scenario starts its own stack, runs it, and tears the stack down. Flags: `--scenario home-sessions,schedules` to pick scenarios, `--web <dist dir>` to reuse a build (or one from another branch's checkout), `--model local|none|fake` to override the scenario's model, `--out <dir>`. The exit code is non-zero if any step failed.

## The stack

```
node tools/qa/stack.ts up --web <dist dir|current> --seed <dir|demo|none> [--apps schedules,slots] [--model local|none|fake]
node tools/qa/stack.ts down
```

`up` prints the URL and stays in the foreground; Ctrl-C or `down` stops it. Everything runs on free ports with a temp data dir, and teardown kills the distributor's whole process group, so a crash leaves no session processes behind (a `kill -9` of `up` itself is the one case that can).

- `--web`: a built `packages/web/dist`, or `current` to build this tree first.
- `--seed`: a directory of session data dirs to copy in (`session.sqlite` and its wal/shm, `session-key`, `agent`, `agent-def`, `meta.json`, `title`, `created`, `touched`; the workspace is skipped), `demo` to create one session (`qa-review`) through a canned model, or `none`.
- `--apps`: mounts the named extensions in the distributor under `/apps/<name>` (they are listed under `extensions` in the deployment file). Schedules is seeded with two sample schedules.
- `--model`: `none` (default) points the deployment's providers at a refused port, but keeps the pinned model names so copied sessions load. `fake` is a canned OpenAI-compatible endpoint, implied by `--seed demo`. `local` uses `QA_MODEL_URL` (default `http://127.0.0.1:11436/v1`, llm-swap with `qwen3.8-27b`) for flows that need a real model; run such a scenario by hand against `up --model local` and click the suggested option on question cards.

## Starter scenarios

| Scenario | Checks |
| --- | --- |
| `home-sessions` | Home, the Sessions list and the session view of a seeded review-style session, at 1280 and 400 px, light and dark |
| `schedules` | The Schedules page against the schedules app with two sample schedules |
| `contracts` | The Contracts empty state, and that New contract opens its dialog (nothing is submitted) |

The demo session is a plain `quack` agent reply shaped like a review, not a GitHub review artifact; use `--seed <dir>` with a copied session to check a real artifact card.

## Add a scenario

Add `v1/tools/qa/scenarios/<name>.ts` that default-exports a `Scenario`: the stack it needs and a `run(ctx)`. Wrap checks in `ctx.step` or `eachView` (which repeats a check at 1280/400 and light/dark), assert with `visible(locator)`, and save images with `ctx.shot(page, name)`.

```ts
import { eachView, visible, type Scenario } from '../scenario.ts';

const scenario: Scenario = {
  name: 'settings',
  stack: {},
  async run(ctx) {
    await eachView(ctx, 'settings page', async (page, label) => {
      await page.goto(`${ctx.url}/#/settings`);
      await visible(page.locator('main').getByText('Settings').first());
      await ctx.shot(page, `settings-${label}`);
    });
  },
};
export default scenario;
```

Routes are hash routes (`#/sessions`, `#/s/<key>`). On a phone width the page title moves into the header, so assert on page content inside `main`, not on an `h1`. A session view scrolls inside `[aria-live="polite"]`, not the window. A question card is `region "Question"` inside `main`; the sidebar Needs-you list repeats its text, so scope to `main`.

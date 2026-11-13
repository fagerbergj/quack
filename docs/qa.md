# QA rig

The "exercise it on a non-production server" check, made repeatable. `tools/qa/` starts a throwaway distributor on free ports, drives the web UI with Playwright at desktop and phone widths in light and dark, and prints a pass/fail line per step with the screenshot paths. A pull request that changes the UI or a page route cites a scenario run in its description (the scenarios run, the summary, and which screenshots you looked at).

## Run

From the repo root:

```
npm ci
npx playwright install chromium      # or: export QA_CHROMIUM=/path/to/chrome
node tools/qa/run.ts --out /tmp/qa-out
```

`run.ts` builds `packages/web` once, then for each scenario starts its own stack, runs it, and tears the stack down. Flags: `--scenario home-sessions,schedules` to pick scenarios, `--web <dist dir>` to reuse a build (or one from another branch's checkout), `--model local|none|fake` to override the scenario's model, `--out <dir>`. The exit code is non-zero if any step failed.

## The stack

```
node tools/qa/stack.ts up --web <dist dir|current> --seed <dir|demo|contracts|artifacts|none> [--apps schedules,slots] [--model local|none|fake]
node tools/qa/stack.ts down
```

`up` prints the URL and stays in the foreground; Ctrl-C or `down` stops it. Everything runs on free ports with a temp data dir, and teardown kills the distributor's whole process group, so a crash leaves no session processes behind (a `kill -9` of `up` itself is the one case that can).

- `--web`: a built `packages/web/dist`, or `current` to build this tree first.
- `--seed`: a directory of session data dirs to copy in (`session.sqlite` and its wal/shm, `session-key`, `agent`, `agent-def`, `meta.json`, `title`, `created`; the workspace is skipped), `demo` to create one session (`qa-review`) through a canned model, `contracts` to write two Contract Studio sessions (`contracts:qa-schedule`, REST, and `contracts:qa-sdk`, TypeScript), each with version 1 approved and version 2 passed and waiting for approval, `artifacts` for those plus sessions holding sealed versions of a GitHub review, two research reports (one with a question waiting) and a Sleeper lineup card, or `none`.
- `--apps`: mounts the named extensions in the distributor under `/apps/<name>` (they are listed under `extensions` in the deployment file). Schedules is seeded with two sample schedules. GitHub gets a throwaway app key and the webhook secret `QA_WEBHOOK_SECRET` from `stack.ts`, so webhooks QA signs reach it while its calls to GitHub are refused.
- `--model`: `none` (default) points the deployment's providers at a refused port, but keeps the pinned model names so copied sessions load. `fake` is a canned OpenAI-compatible endpoint, implied by `--seed demo`. `local` uses `QA_MODEL_URL` (default `http://127.0.0.1:11436/v1`, llm-swap with `qwen3.8-27b`) for flows that need a real model; run such a scenario by hand against `up --model local` and click the suggested option on question cards.

## Starter scenarios

| Scenario | Checks |
| --- | --- |
| `home-sessions` | Home, the Sessions list and the session view of a seeded review-style session, at 1280 and 400 px, light and dark |
| `schedules` | The Schedules page against the schedules app with two sample schedules |
| `contracts` | The Contracts empty state, and that New contract opens its dialog (nothing is submitted) |
| `subagent` | A QA-only agent (written beside a copy of `agents/`, shipped bundles untouched) that selects `subagent` runs one call through a scripted fake model; `GET /messages?conversation=` serves the child, and the call row nests the child's messages, at 1280 and 400 px, light and dark |
| `contract-studio` | A seeded contract beside its chat (tabs on a phone) with the changes since version 1 highlighted, the class diagram re-centring on a type, and approving the waiting version |
| `artifact-panes` | Each artifact kind (review, report, contract, Sleeper card) beside its chat at 1280 and 1920 px and on its own tab at 400 px, light and dark, where the shown tab panel (pane or chat) fills the width; a pending question opening the tab where it is answered; the divider's keyboard resize surviving a reload; an artifact's own page from the Artifacts list with Back and Forward; the header PR chip's 24 px hit area |
| `needs-you` | Home's Needs you answers the seeded research question at Pixel 7 width, dark, with an option and a note: the card goes, the ask reads resolved with the note as its reason, and the page logs no errors |
| `search-filter` | The one search-and-filter row on Sessions and Artifacts: the funnel's panel (popover at 1280 px, sheet at 400 px), a filter set from it answered by the server, and its removable chip, light and dark |
| `pr-chips` | Four github review sessions whose PR badges are open, merged and closed (from signed webhooks to the github app) and unknown (a repo the app is not installed on), in the Sessions list and each session's header; a badge links to its pull request, takes focus, and opens a new tab without opening the session, at 1280 and 400 px, light and dark |
| `pr-live` | A github review session in the Sessions list whose badge is open after a signed `reopened` webhook, then flips to merged on a signed `closed` webhook through the app's `pull_request` stream, with no re-read, at 1280 and 400 px, light and dark |
| `home-read-session` | The home agent's `read_session`, called through the fake model with the home session's own credential: a seeded session's sealed report leads the read in full with no messages, and a researcher session read with `messages: true` returns its last messages (no browser steps) |
| `judge-loop` | QA-only research agents enabling the judge (2 and 0 revisions), with the fake model writing and judging: a failing report comes back and its revision passes, a failing report at the limit goes out with its failures marked, and another seal past the limit ends the run; the passed report in its pane at 1280 px |

The demo session is a plain `quack` agent reply shaped like a review, not a GitHub review artifact; use `--seed artifacts`, or `--seed <dir>` with a copied session, to check a real artifact card.

## Add a scenario

Add `tools/qa/scenarios/<name>.ts` that default-exports a `Scenario`: the stack it needs and a `run(ctx)`. Wrap checks in `ctx.step` or `eachView` (which repeats a check at 1280/400 and light/dark), assert with `visible(locator)`, and save images with `ctx.shot(page, name)`.

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

Routes are hash routes (`#/sessions`, `#/s/<key>`). On a phone width the page title moves into the header, so assert on page content inside `main`, not on an `h1`. A session view scrolls inside its pane, not the window; find the transcript as `getByRole('log', { name: 'Transcript' })`. A question card is `region "Question"` inside `main`; the sidebar Needs-you list repeats its text, so scope to `main`.

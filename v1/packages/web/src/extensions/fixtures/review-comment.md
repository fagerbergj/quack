## Takeaway
A well-tested, well-delivered rebuild of the Sleeper page into one scroll with a week stepper and a Run a job sheet; approve-track with a few clarifications — most notably the issue's "Analyze a trade…" entry, which is left out without a cited follow-up.

## Verdict
comment

## Findings
- suggestion: v1/packages/web/src/extensions/sleeper/week.ts:64 — the history fallback for a past week's result drops `opp`, which the history row carries (every row in `fixtures/8-history.json` has `opp`). The retro path renders "140.28 to 99.68" but the fallback renders a bare "140.28", so the same block reads as a full score in one path and a bare points total in the other.
```ts
return row && { won: row.result === 'W' ? true : row.result === 'L' ? false : undefined, started: row.started, opp: row.opp, left: row.left, best: row.best };
```
- question: v1/packages/web/src/extensions/sleeper/Sleeper.tsx:220 — `current = weeks.at(-1)` makes "the current week" the newest week with any session, not Sleeper's live week. Before any job runs this week (fresh week, or a bye week with no runs), the previous week renders as current — with its stale lock pin, the Run button, and › disabled — and the live week is invisible until a session appears. Self-correcting within one rows refresh after a run, but please confirm this is the intended reading of "current week" rather than something the schedules follow-up should also fix.
- question: v1/packages/web/src/extensions/sleeper/Sleeper.tsx:147 — issue #2007's Fix list has "Analyze a trade… opens its form" in the Run a job sheet; the sheet's `JOBS` has no such entry and the PR's "Left out" defers it (no picker reads) without citing a follow-up issue. Is there one, or does this bullet of the issue stay open intentionally?
- suggestion (tests): v1/packages/web/src/extensions/sleeper/Sleeper.test.tsx:127 — the stepper test clicks "Earlier week" twice and checks "Later week" is enabled on a past week, but never clicks it, so `next = weeks.find((w) => w > (week ?? 0))` (Sleeper.tsx:237) is unexercised; the "Nothing from this week." branch (Sleeper.tsx:244) — a past week whose sessions have no sealed results — is also untested. A broken `next` direction would pass the suite as written.
- nit: v1/packages/web/src/extensions/sleeper/cards.tsx:14 — `unid` strips any 4–5 digit number followed by a letter, so prose like "the 2026 draft class" loses "2026 " as well. Anchoring on the name that follows (or requiring the id to be at a token boundary the model actually emits) would avoid eating years.
- nit: v1/packages/web/src/extensions/sleeper/cards.tsx:27 — `titleOf`'s `default` returns 'League history', so any future job kind rendered through this switch would be mislabeled; `r.job` would fail visibly instead.

## Verified
- Read the full diff against origin/v1 plus the surrounding code: `service.ts` (page route, `startOnDemand`, `historyParams`), `week.ts`, `cards.tsx`, `parts.tsx`, `Sleeper.tsx`, `api.ts`, `CheckList.tsx`, all seven fixtures, and the extension wiring (`index.ts`, `App.tsx`, `Workspace.tsx`).
- Re-ran locally after `npm ci`: web vitest 95/95 passed (including the new `Sleeper.test.tsx`, 16 tests, and the updated `App.test.tsx`); sleeper service tests 128/128 passed, including the extended page-route test (waivers 202 under the week key, `history?seasons=x` → 400); `npm run typecheck` clean; `node tools/sloplint.ts diff origin/v1` clean.
- CI: the `check` was still `queued` at review time, so CI's own result is not evidence here; the local runs above stand in for it.

## Notes
- The deferrals are honestly marked: scheduled-run pins carry the shortcut comment (Sleeper.tsx:84), and the trade card renderer's non-empty `offers` path (cards.tsx:85-90) has no sealed fixture — only the no-deal finder shape is exercised, as the PR discloses.
- Current-week views deliberately exclude `trade` (analyzed-trade) sessions — `CURRENT` and `wanted` list `trade-finder` only — so a trade analyzed mid-week stays off the page until the week becomes a past week, where `PAST` does include `trade`.
- The fixtures are real sealed prod results and the tests assert the promised invariants directly (no player ids, no version numbers or "card" in visible text, 44 px targets via `min-h-11`), which is a strong test base for the new branches.

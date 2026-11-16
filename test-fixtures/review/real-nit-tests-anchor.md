## Takeaway
No code changed since the last review — same head `d529e69`, same red `npm test` CI run, and the one open test nit stands unchanged with no reply — so the verdict is unchanged: approve once the nit is addressed or dismissed and the CI run is green or explained.

## Verdict
comment

## Findings
- nit (tests): packages/distributor/test/apps.test.ts:117 — still open from the previous review; the line is unchanged and there is no reply. The default-namespace path is tested only negatively: an extension event reaches a no-`listen` listener only when `type` starts with the listener's own name and `installed(session).has(l.name)` (packages/distributor/src/apps.ts:42, :53), but in this test the `has` true-case is only ever exercised through listener `a`, which has a `listen`, while listener `b` (no `listen`) is only seen being filtered out. A regression in the prefix rule or the installed gate for no-`listen` apps would pass this test and leave such apps deaf to their own session's events. Give `b` a session where it is installed and assert it is delivered:

```ts
const stream = appStream(async (session) => new Set(session === 'mine' ? ['a', 'b'] : []));
// ...
assert.deepEqual(heard, ['a other run.ended', 'a mine b.done', 'b mine b.done']);
```

## Verified
- Re-reviewed at head d529e69, byte-identical to the head of the previous review (`git diff origin/main...HEAD` matches the 17-file stat exactly); the feature files carry the earlier per-file test runs.
- Re-ran the in-scope tests today at this head: distributor apps, home notices, home start, catalog channel — 25/25 pass.
- Prior full verification at this head stands: `npm run typecheck` pass, `npm run sloplint -- diff origin/main` pass, full non-web `node --test` suite 1059 pass / 1 fail (a remarkable ingest-ordering flake untouched by this diff that passes 3/3 in isolation; `tools/qa-stack.test.ts`'s port-relaunch test times out in this docker-less sandbox and its own comment cites an earlier CI timeout of the same file).
- GitHub API: the only workflow run for this head is 43493614104 (attempt 1); `check` failed at `npm test` with both tsc steps and `isolation` green. No re-run has been posted since the previous review.
- Prior inline comment on apps.test.ts:117: no reply, and the commented line is unchanged in the current diff, so the nit is carried over rather than closed.

## Notes
- CI: the same `npm test` failure stands with no new run to inspect. All in-scope tests pass locally, and the only local failures are pre-existing flakes outside the diff. If a re-run stays red, please name the failing test — if it is in a file this PR touches, that changes this review; if it is one of those flakes or the web suite, approve is my expectation given the in-scope evidence.
- The home app's `notices`/`started` tables (packages/home/src/notices.ts:23) grow one row per run end of a Home-started session and are never pruned. Fine at v1 scale; worth a cleanup follow-up if run volume grows.
- "Fixes #2069" will close the issue whose last acceptance item ("the result appears in the conversation when it lands") only holds on prod after the deploy adds `extensions.home: {}` to `v1-deployment.json`. The graceful degradation is in the diff — make sure the entry ships with this.

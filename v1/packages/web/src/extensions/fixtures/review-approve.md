## Takeaway
The head moved to address both findings from the first pass: `record` now retries a failed direct-spend commit (the blocking underreport), and a loop+direct sum test was added; all gates are green, so this is ready to merge.

## Verdict
approve

## Findings
- nit: v1/packages/index/src/publisher.ts:52 — the `record` retry loops with no delay between attempts. For a truly transient Outbox blip this is fine and the bounded 3 attempts are correct, but a real storage hiccup that outlasts a few milliseconds will consume all three attempts back-to-back and give up. A small backoff would make the retry more likely to recover without changing the happy path. Not blocking — pi-durable's guidance is only "retry transient failures," and the failure still surfaces via the outer `.catch`.

```ts
    } catch (err) {
      if (n >= tries) throw err;
      await new Promise((r) => setTimeout(r, 50)); // let a transient blip clear before the next attempt
    }
```

## Verified
- The working tree arrived at a stale commit (`f1dfd9f`); the PR is actually two cost commits (`0c0b8b2f` feature, `a6e9e692` retry fix) on base `9c37ad05`, not the live `origin/v1`. Checked out `a6e9e692` and diffed `9c37ad05...a6e9e692` (6 files, +107/-15) — this is the real PR.
- Ran the index publisher+cost tests: 8/8 pass, including the 4 new ones (`a priced direct completion ... 0.25`, `an unpriced ... 0`, `a direct-spend commit that fails once is retried, so the spend is still recorded`, `loop and direct spend are summed into one cost fact`) and the pre-existing `cost.test.ts` "the cost fact includes the spend of a decision fork".
- Ran the full node suite `packages/*/test/*.test.ts tools/*.test.ts`: 440 pass, 4 skipped (postgres, no `QUACK_TEST_DATABASE_URL`), 0 fail — so the remarkable rmc tests the description warns about did not fail here.
- `npm run typecheck` exit 0; `node tools/sloplint.ts diff 9c37ad05` → `sloplint: ok`; LSP clean on publisher.ts and facts.ts.
- Confirmed the retry test is real: the Proxy makes the first `root.commit` reject while `failures=1`, and the direct-spend `record` commit is the next commit after `startPublisher` (the `observeGeneration`/`completeSimple` path fires no harness event and `setStatus`/ack commits run afterward with `failures` already 0), so the blip is isolated to `record`'s first attempt; without the retry the spend would drop and the assertion would fail.
- Re-traced the fixed `costFact` (facts.ts:54): `t` seeds from `direct` (or zeros), adds the `harness.usage` totals, `rev` = tokens + micro-dollars (both monotonic), published when `rev > 0`; the combined test asserts the published token sum equals the measured loop tokens + the direct 125 and `total === 0.25`, so it would fail if either source overwrote the other.
- Compared the new instances to their siblings: the zero-initialized `Spend` objects in `record` (publisher.ts:56) and `costFact` (facts.ts:55) both carry exactly the six fields of the `Spend` type (input, output, cacheRead, cacheWrite, cost, at), and `priced(l)` (definition.ts:141) carries all four `Price` fields (definition.ts:12) that pi-ai's `ModelCost` requires — no sibling field is missing; `tiers` is the only `ModelCost` field absent and it is optional.

## Notes
- The retry closes the transient-failure window, but a *fatal* storage error (which, per pi-durable, closes the Session and is not retried) still loses the in-flight completion's spend — the commit was never written, so a reopen from storage does not restore it. That is inherent to the storage layer and affects pi-durable's own usage totals equally, not specific to this path; the PR's "Durable" claim holds for restarts and log rebuilds where the storage is healthy.
- `onGeneration`/`observeGeneration` are both exported from `@quack/sdk` (signals.ts:32/37) and the `Spend` type is now exported from facts.ts so the publisher can name its parameter; the `direct` field stays optional so existing deployments (no direct completions) are unaffected.

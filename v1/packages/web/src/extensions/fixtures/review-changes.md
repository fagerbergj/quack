## Takeaway
Clean, well-scoped per-run cost implementation that keeps every promise in #2001 (tests, typecheck, sloplint all pass), but the direct-spend path silently underreports cost when its Outbox commit fails, so it needs a bounded retry (or a corrected durability claim) and a test for the loop+direct sum.

## Verdict
request_changes

## Findings
- blocking (defect): v1/packages/index/src/publisher.ts:58 — if `root.commit` for the direct-spend record rejects, the trailing `.catch((e) => console.error(...))` swallows it and that completion's tokens/cost never land in `doc.direct`. `costFact` (facts.ts:54) derives the published cost from `outbox.direct`, so the index then reports a permanently lower cost than the run actually incurred — a silent underreport for a cost feature — and a restart or log rebuild bakes the loss in, contradicting the PR's "Durable, so a restart or log rebuild keeps it" claim. pi-durable's own docs say a failed commit is final and to "retry transient failures inside the method," so a transient Outbox blip drops the spend today rather than recovering it.

```ts
// A failed commit is final (pi-durable), so retry a transient Outbox blip here instead of dropping the spend.
const record = async (u: NonNullable<AssistantMessage['usage']>, tries = 3): Promise<void> => {
  let last: unknown;
  for (let n = 0; n < tries; n++) {
    try {
      await root.commit(async (tx) => {
        const doc = await tx.doc(Outbox, root.id);
        doc.direct ??= { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0 };
        const d = doc.direct;
        d.at = now();
        d.input += u.input; d.output += u.output; d.cacheRead += u.cacheRead; d.cacheWrite += u.cacheWrite; d.cost += u.cost?.total ?? 0;
      }, ctx);
      return;
    } catch (e) { last = e; await new Promise((r) => setTimeout(r, 50)); }
  }
  throw last; // still failing: let the caller's .catch surface it
};
// listener becomes: spending = spending.then(() => record(u)).then(() => flush()).catch((e) => console.error('index: direct spend', e));
```

- suggestion: v1/packages/index/test/publisher.test.ts:105 — the two new tests exercise direct completions only (`openDemo` produces zero loop usage), and the pre-existing `cost.test.ts` exercises loop/fork usage only (no direct). Neither has both sources at once, so a regression in `costFact`'s combination (e.g. `direct`'s tokens replacing the loop totals, or the loop overwriting `direct`) would not be caught. Add a test where a session has a loop generation AND a direct completion and assert the loop is added on top of the direct baseline (125 tokens).

```ts
test('loop and direct spend are summed into one cost fact', async () => {
  const index = await stubIndex();
  index.state.mode = 'ok';
  const session = await openDemo();
  const publisher = await startPublisher(session, { url: index.url, token: 't', meta, pollMs: 60_000 });
  await session.submit('Hi', 'c1', true);                        // a loop generation (faux provider reports tokens)
  await observeGeneration({ messages: [] }, async () => reply(0)); // a direct completion (125 tokens, cost 0)
  await publisher.stop();
  const c = index.state.data.get('cost')!;
  assert.ok(c.input + c.output + c.cacheRead + c.cacheWrite > 125, 'loop tokens are added on top of the direct 125');
});
```

## Verified
- Ran `npm install`, then the index and session suites. The two new publisher tests (`a priced direct completion publishes its tokens and cost 0.25`, `an unpriced ... cost 0`) and the new definition price test pass; the pre-existing `cost.test.ts` "the cost fact includes the spend of a decision fork" still passes. 14/14 index tests (4 postgres skipped: no `QUACK_TEST_DATABASE_URL`), 11/11 session `definition.test.ts`.
- `npm run typecheck` clean; `node tools/sloplint.ts diff origin/v1` → `sloplint: ok`; no LSP diagnostics on the four changed `src` files.
- Read pi-ai to confirm the "no cost arithmetic of our own" claim: `calculateCost` (models.js:672) divides the model's `cost` rates by 1e6 (USD per million) and fills `usage.cost.{input,output,cacheRead,cacheWrite,total}`; openai-completions.js:1198 calls it, so a model fed `priced()` reports per-call cost. `ModelCost` = `{input,output,cacheRead,cacheWrite,tiers?}`, which `priced()` satisfies.
- Read pi-durable types: "Session serializes commits" (no race between the direct-spend commit and the `status`/ack commits), and a failed commit is final with "retry transient failures inside the method" — the basis for the blocking finding above.
- Confirmed the loop/direct partition is complete and non-overlapping: `harness.usage` covers the generation task, compaction, tool usage and `sessionModel` forks (via `fork.submit`); the `completeSimple` callers (workflows/steps.ts:76, sdk/decide.ts:109, remarkable ingest) bypass pi-durable and are heard through the global `onGeneration` hook into `direct`. `startPublisher` runs once per process (runtime/main.ts:26, A1 one-process-one-session), so the module-global listener is safe and `stop()`'s `unhear()` prevents a leak.
- `rev = tokens + Math.round(cost*1e6)` is a non-negative safe integer and monotonic (tokens and cost only grow), satisfying the index's `Number.isSafeInteger` gate (http.ts `isFact`); the store's `tokens`/`cost` both default to 0 when the fact is unpublished, so an unpriced model still surfaces tokens.

## Notes
- `stop()` calls `unhear()` before `await spending`, so a completion still in flight at SIGTERM never records its usage. Shutdown-only, and the process is ending regardless. publisher.ts:73-74.
- The `cost` fact's `at` is `direct.at` when any direct spend exists, else 0, so the mixed case dates the whole fact by the last workflow completion rather than the last loop call. Disclosed in "Left out" and harmless: last-activity is driven by the `status` fact.
- `costFact` guards `u.cost?.total ?? 0` but not the token fields; that matches the `Usage` type, where `input/output/cacheRead/cacheWrite` are required and only `cost` is realistically absent (tool buckets). Correct as written.

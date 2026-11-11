import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { buildEvidence } from '@quack/judge';
import { parseReview, reviewDeclaration, reviewKind, resolveCitedPath, reviewInputs, reviewerRubric, validateReview } from '../src/index.ts';
import { commentableLines, splitReview } from '../src/inline.ts';
import { namedSources } from '../src/rubric.ts';

const review = `## Takeaway
Fix the double-counted cache tokens before merging.
## Verdict
request_changes
## Findings
- blocking (defect): internal/eval/replay.go:214 — adds cached_input to input, which already includes it.
  \`\`\`go
  total := in.Input
  \`\`\`
- nit: \`internal/cli/eval.go:88\` rename the flag
- question: no citation here?
## Verified
- read replay.go:200-230
`;

test('parses sections, findings with citations, anchors and continuation lines', () => {
  const r = parseReview(review);
  assert.equal(r.verdict, 'request_changes');
  assert.deepEqual(r.findings.map((f) => [f.label, f.anchor, f.path, f.line]), [
    ['blocking', 'defect', 'internal/eval/replay.go', 214],
    ['nit', undefined, 'internal/cli/eval.go', 88],
    ['question', undefined, undefined, undefined],
  ]);
  assert.match(r.findings[0].text, /```go\n {2}total := in.Input/);
  assert.match(r.sections.verified, /read replay.go/);
});

test('validation reports every structure problem at once', () => {
  assert.deepEqual(validateReview(review), []);
  const problems = validateReview('## Verdict\nmaybe\n## Findings\n- praise: nice\n');
  assert.deepEqual(problems, ['missing a "## Takeaway" section', 'the verdict must be one of approve, comment, request_changes', 'finding 1: label must be one of blocking, suggestion, nit, question']);
});

const facts = (f: object) => ({ facts: [{ id: 'facts', state: { facts: f } }] }) as never;
const code = (criterion: string, id: string) => reviewerRubric.criteria.find((c) => c.id === criterion)!.checks.find((k) => k.id === id)!.code!;

test('the verdict must match the findings labels', () => {
  const verdict = code('structured_verdict', 'verdict_matches_labels');
  assert.equal(verdict(review, {}).met, 1);
  assert.equal(verdict(review.replace('request_changes', 'approve'), {}).met, 0);
  const questionOnly = '## Takeaway\nx\n## Verdict\ncomment\n## Findings\n- question: a.ts:1 why?\n';
  assert.equal(verdict(questionOnly, {}).met, 1);
  assert.equal(verdict(questionOnly.replace('comment', 'approve'), {}).met, 0);
  const clean = '## Takeaway\nx\n## Verdict\ncomment\n## Findings\n## Verified\n- could not run the tests offline\n';
  assert.equal(verdict(clean, {}).met, 1, 'comment is allowed when verification could not finish');
  assert.equal(verdict(clean.replace('comment', 'approve'), {}).met, 1, 'and approve is still right beside it');
  for (const gap of ['- the race test was not run here', "- didn't reproduce it on arm64", '- unable to verify the migration'])
    assert.equal(verdict(clean.replace('- could not run the tests offline', gap), {}).met, 1, `a disclosed gap, however phrased: ${gap}`);
  assert.equal(verdict(clean.replace('could not run the tests offline', 'ran the tests: all pass'), {}).met, 0, 'no gap, no comment');
  const green = '## Takeaway\nx\n## Verdict\napprove\n## Findings\n';
  const ci = (s: string) => facts({ ci: s, findings: [] });
  assert.equal(verdict(green, ci('go-test=success')).met, 1);
  assert.equal(verdict(green, ci('go-test=success, lint=failure')).met, 0, 'a failing check is never approved past');
  assert.equal(verdict(green.replace('approve', 'comment'), ci('lint=failure')).met, 1);
  for (const failed of ['stale', 'startup_failure', 'timed_out']) assert.equal(verdict(green, ci(`go-test=${failed}`)).met, 0, `${failed} is a failed check`);
  for (const running of ['queued', 'in_progress', 'waiting']) {
    assert.equal(verdict(green, ci(`go-test=${running}`)).met, 1, `approve while ${running} is allowed`);
    assert.equal(verdict(green.replace('approve', 'comment'), ci(`go-test=${running}`)).met, 1, `${running} CI is not green, so comment is allowed too`);
  }
  assert.equal(verdict(green.replace('approve', 'comment'), ci('go-test=success')).met, 0, 'green CI with nothing open still calls for approve');
});

test('blocking findings must name an anchor, and findings must cite lines that exist', () => {
  const anchor = code('severity_grounded', 'blocking_names_anchor');
  assert.equal(anchor(review, {}).met, 1);
  assert.match(anchor(review.replace('blocking (defect)', 'blocking'), {}).detail!, /finding-1/);
  const cites = code('claims_grounded', 'cites_existing_lines');
  const two = cites('', facts({ ci: '', findings: [{ id: '1', cited: true }, { id: '2', cited: false }] }));
  assert.deepEqual([two.met, two.detail], [0.5, 'no existing path:line cited in finding-2']);
  assert.equal(cites('', facts({ ci: '', findings: [] })).met, 1);
});

test('approval_grounded judges an approve on the review alone, and has no item on any other verdict', async () => {
  const evidence = async (verdict: string) => (await buildEvidence(reviewDeclaration, await reviewInputs()({ snapshot: async () => undefined, conversationId: 'c' } as never, { content: `## Takeaway\nx\n## Verdict\n${verdict}\n## Findings\n` } as never, {} as never))).evidence;
  const approve = await evidence('approve');
  assert.deepEqual(approve.approve, [{ id: 'approve', state: { review: '## Takeaway\nx\n## Verdict\napprove\n## Findings\n' } }]);
  for (const v of ['request_changes', 'comment']) assert.deepEqual((await evidence(v)).approve, []);
});

test('each finding is an item with its cited code, sorted by label, all sharing the CI result; the description splits at its headings', async () => {
  const body = 'Adds a flag.\n\n## What changed\n- the flag\n\n## Verification\n- npm test: ok\n\n### Rollout\nbehind a setting';
  const pr = { owner: 'acme', repo: 'widgets', number: 7, title: 't', body, baseRef: 'main', headSha: 'abc', checks: [{ name: 'ci', status: 'completed', conclusion: 'success' }], linkedIssues: [{ number: 3, title: 'flag', body: 'we need a flag' }] };
  const api = { conversationId: 'c', snapshot: async () => ({ pr }), env: { exec: async () => ({ ok: true, value: { exitCode: 0 } }), readTextFile: async () => ({ ok: true, value: 'const a = 1;\nconst b = 2;\n' }) } };
  const inputs = await reviewInputs()(api as never, { content: review } as never, {} as never);
  const built = await buildEvidence(reviewDeclaration, inputs);
  assert.deepEqual(built.evidence.blocking.map((u) => u.id), ['blocking 1']);
  assert.deepEqual(built.evidence.nonblocking.map((u) => u.id), ['nonblocking 2', 'nonblocking 3']);
  assert.deepEqual(built.evidence.finding[0], { id: 'finding 1', state: { finding: { text: 'blocking (defect): internal/eval/replay.go:214 adds cached_input to input, which already includes it.\n  ```go\n  total := in.Input\n  ```', citedCode: '(the cited lines do not exist)' } }, shared: { ci: 'ci=success' } });
  assert.deepEqual(built.evidence.promise.map((u) => [u.id, u.state.part]), [
    ['promise opening', 't\n\nAdds a flag.'],
    ['promise What changed', '## What changed\n- the flag'],
    ['promise Rollout', '### Rollout\nbehind a setting'],
    ['promise Linked issue #3: flag', '## Linked issue #3: flag\nwe need a flag'],
  ], 'what was run to verify it promises nothing, so it is skipped');
  assert.deepEqual(Object.keys(built.evidence.promise[0].shared!), ['diff', 'review'], 'the parts share the diff and the review');
  assert.deepEqual(built.evidence.verified.map((u) => u.state.item), ['read replay.go:200-230']);
});

test('every model check is declared; a pitfall is asked as the defect and costs the share of items it flags', () => {
  const checks = reviewerRubric.criteria.flatMap((c) => c.checks);
  assert.deepEqual(checks.filter((k) => !k.code).map((k) => k.id).sort(), Object.keys(reviewDeclaration.checks).sort());
  for (const k of checks.filter((x) => !x.code)) {
    const d = reviewDeclaration.checks[k.id];
    const pitfall = typeof k.weight === 'number' && k.weight < 0;
    // A Verified item is asked as the defect too, so a line that only lists what was read holds.
    const asDefect = pitfall || k.id === 'verified_names_evidence';
    assert.deepEqual([d.question.type === 'bool' && d.question.pass, pitfall && d.aggregate.name], [asDefect ? ['no'] : undefined, pitfall ? 'share' : false], k.id);
  }
  assert.ok(checks.some((k) => k.id === 'blocks_without_consequence' && k.weight === -4));
  const possible = reviewerRubric.criteria.flatMap((c) => c.checks).reduce((n, k) => n + (typeof k.weight === 'number' && k.weight > 0 ? k.weight : 0), 0);
  assert.equal(possible, 24);
});

test('an anchor without its space still parses, a cited root file counts, and an unreadable finding line is a problem', () => {
  const r = parseReview('## Findings\n- blocking(defect): src/a.ts:1 — wrong total\n- nit: Dockerfile:12 — pin the base\n- blocker src/b.ts:2 nope\n');
  assert.deepEqual(r.findings.map((f) => [f.label, f.anchor, f.path, f.line]), [['blocking', 'defect', 'src/a.ts', 1], ['nit', undefined, 'Dockerfile', 12]]);
  const prose = parseReview('## Findings\n- question: node:18 is end of life; why not 20?\n- nit: go.mod:3 — bump\n- nit: ./x/y.go:4 — z\n');
  assert.deepEqual(prose.findings.map((f) => [f.path, f.line]), [[undefined, undefined], ['go.mod', 3], ['./x/y.go', 4]]);
  const problems = validateReview('## Takeaway\nx\n## Verdict\napprove\n## Findings\n- blocker src/b.ts:2 nope\n');
  assert.deepEqual(problems, ['not a finding line ("- label (anchor): path:line — text"): - blocker src/b.ts:2 nope']);
});

test('a failing git diff fails the inputs instead of scoring the review on no diff', async () => {
  const pr = { owner: 'acme', repo: 'widgets', number: 7, title: 't', body: '', baseRef: 'main', headSha: 'abc', checks: [], linkedIssues: [] };
  const api = (exitCode: number) => ({ conversationId: 'c', snapshot: async () => ({ pr }), env: { exec: async (_argv: string[], o: { onOutput?: (t: string) => void }) => (o?.onOutput?.(exitCode ? 'fatal: bad revision origin/main...HEAD' : ''), { ok: true, value: { exitCode } }) } });
  const sealed = { content: '## Takeaway\nx\n## Verdict\napprove\n## Findings\n' } as never;
  await assert.rejects(reviewInputs()(api(128) as never, sealed, {} as never), /git diff in repo failed: fatal: bad revision/);
  assert.equal((await reviewInputs()(api(0) as never, sealed, {} as never)).diff, '');
});

test('a cited path resolves against tracked files: checkout prefix stripped, then a unique tail (prod cases)', () => {
  const files = ['v1/packages/web/src/a.ts', 'v1/packages/slots/test/slots.test.ts', 'v1/x/index.ts', 'v1/y/index.ts', 'README.md'];
  assert.equal(resolveCitedPath(files, 'repo', 'repo/v1/packages/web/src/a.ts'), 'v1/packages/web/src/a.ts');
  assert.equal(resolveCitedPath(files, 'repo', 'packages/slots/test/slots.test.ts'), 'v1/packages/slots/test/slots.test.ts');
  assert.equal(resolveCitedPath(files, 'repo', 'index.ts'), undefined, 'two tails is ambiguous');
  assert.equal(resolveCitedPath(files, 'repo', 'ots.test.ts'), undefined, 'a tail must start at a / boundary');
  assert.equal(resolveCitedPath(files, 'repo', 'packages/nope.ts'), undefined);
});

const DIFF = `diff --git a/src/a.ts b/src/a.ts
--- a/src/a.ts
+++ b/src/a.ts
@@ -1,3 +1,4 @@
 one
-two
+two!
+three
 four
`;

test('commentable lines are the added and context lines of the new file', () => {
  assert.deepEqual([...commentableLines(DIFF).get('src/a.ts')!], [1, 2, 3, 4]);
});

test('findings in the diff go inline; the rest and the sections stay in the body', () => {
  const text = `## Takeaway
Two things.
## Verdict
comment
## Findings
- suggestion (design): a.ts:2 — Rename it. The new name says what it holds.
  \`\`\`ts
  const kept = 1;
  \`\`\`
- blocking (defect): src/a.ts:90 — Off the diff.
## Verified
- read it
`;
  const { body, comments } = splitReview(text, DIFF, ['src/a.ts'], 'repo');
  assert.deepEqual(comments, [{ path: 'src/a.ts', line: 2, body: '**suggestion (design)**: Rename it.\n\nThe new name says what it holds.\n  ```ts\n  const kept = 1;\n  ```' }]);
  assert.equal(body, `## Takeaway
Two things.
## Verdict
comment
## Findings
- blocking (defect): src/a.ts:90 — Off the diff.

Posted inline on the diff:
- suggestion: Rename it.
## Verified
- read it
`);
  assert.equal(splitReview(text, '', ['src/a.ts'], 'repo').comments.length, 0);
});

test('a long first sentence stays whole in the inline comment and is shortened only in the body list', () => {
  const long = `Rename ${'the thing that holds the value '.repeat(6)}so it reads well.`;
  const text = `## Takeaway\nx\n## Verdict\ncomment\n## Findings\n- nit: src/a.ts:2 — ${long} Then more.\n## Verified\n- y\n`;
  const { body, comments } = splitReview(text, DIFF, ['src/a.ts'], 'repo');
  assert.equal(comments[0].body, `**nit**: ${long}\n\nThen more.`);
  assert.match(body, /- nit: Rename .*…\n## Verified/);
});

test('the review kind reads its outcome from the verdict, as the review is posted', () => {
  assert.equal(reviewKind.outcome?.(review), 'request_changes');
  assert.equal(reviewKind.outcome?.('# Review\nfine\n'), undefined);
});

test('a Verified item names its source when it shows a command, file:line, CI check, commit or URL; an absence claim needs its search', async () => {
  // The 77 Verified items of the replay set (14 recorded reviews and a re-review), each label checked by hand.
  const items: Array<{ item: string; backed: boolean; text: string }> = JSON.parse(readFileSync(new URL('./fixtures/verified-items.json', import.meta.url), 'utf8'));
  assert.equal(items.length, 77);
  assert.deepEqual(items.filter((it) => (namedSources(it.text) > 0) !== it.backed).map((it) => it.item), []);
  const ran = ['Ran `cargo test -p judge --release`: 41 pass.', 'Ran `FOO=1 make check` clean.', "`pnpm -r exec 'tsc --noEmit'` is clean."];
  const read = ['`const a = 2;` is unchanged.', 'Read `onEvent` and its callers.', 'The logic is correct and the tests pass.', 'Confirmed `legacy` is referenced nowhere.'];
  assert.deepEqual([...ran, ...read].map((t) => namedSources(t) > 0), [true, true, true, false, true, false, false], 'a command line or the code read counts; a statement, a bare claim or an unsearched absence does not');
  const checks = ['build', 'e2e'];
  assert.deepEqual(['`e2e` passed on this head.', 'build=success.', 'The build passed.'].map((t) => namedSources(t, checks) > 0), [true, true, false], "the PR's own checks count when cited, not as bare words");
  const absent = ['Nothing imports `legacy` any more.', 'No callers of `x` are left.', '`rg legacy` finds nothing; no imports remain.', 'Searched the tree: nothing references `x`.'];
  assert.deepEqual(absent.map((t) => namedSources(t) > 0), [false, false, true, true], 'an absence claim is backed only by its search');
  const built = await buildEvidence(reviewDeclaration, { review: '## Verified\n- CI `check` is green on this head.\n- Confirmed `x` is referenced nowhere.\n' }, undefined, (s) => s.piece === 'item');
  assert.deepEqual(built.evidence.verified.map((u) => [u.id, u.related]), [['verified item-1', { sources: 2 }], ['verified item-2', { sources: 0 }]]);
  assert.deepEqual(reviewDeclaration.checks.verified_names_evidence.aggregate, { name: 'share', by: 'either' }, 'a named source settles the item; the rest go to Clef');
});

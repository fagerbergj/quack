import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildEvidence } from '@quack/judge';
import { parseReview, reviewDeclaration, reviewKind, resolveCitedPath, reviewInputs, reviewerRubric, validateReview } from '../src/index.ts';
import { commentableLines, splitReview } from '../src/inline.ts';
import { claimedNames, dependencyDirs, misquotes, quotedCode } from '../src/review.ts';

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
  const two = cites('', facts({ ci: '', findings: [{ id: '1', cited: true, misquotes: [] }, { id: '2', cited: false, misquotes: [] }] }));
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
    assert.deepEqual([d.question.type === 'bool' && d.question.pass, pitfall && d.aggregate.name], pitfall ? [['no'], 'share'] : [undefined, false], k.id);
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

test('statement-like inline quotes are collected for checking; identifiers, short spans and fenced proposals are not', () => {
  const text = "`opponent()` finds none, so `addWeekForm` appends `teamWeek{Week: w, Points: 0, Result: \"-\"}`; see `className=\"${CHIP} bg-blue-100 ...\"`.\n```go\nx := teamWeek{Points: 1}\n```";
  assert.deepEqual(quotedCode(text), [['teamWeek{Week: w, Points: 0, Result: "-"}'], ['className="${CHIP} bg-blue-100']]);
  const gate = code('claims_grounded', 'quotes_exist');
  assert.equal(gate('', facts({ ci: '', findings: [{ id: '1', cited: true, misquotes: [] }] })).met, 1);
  const failed = gate('', facts({ ci: '', findings: [{ id: '1', cited: true, misquotes: [] }, { id: '2', cited: true, misquotes: ['Points', 'x'] }] }));
  assert.equal(failed.met, 0.5);
  assert.match(failed.detail!, /finding-2 names identifiers found nowhere in the repository, the diff or its dependencies: `Points`, `x`/);
  assert.match(failed.detail!, /command output or logs go in a fenced block/);
  assert.deepEqual(quotedCode('it fails:\n```text\npanic: runtime error: x == nil {\n```'), [], 'quoted output in a fence is not checked');
});

test('a quote is invented only when a name it claims is in no repo file, diff or installed dependency (prod cases)', async () => {
  const has = (corpus: string, argv: string[]) => new RegExp(`(?<![\\w$])${argv[argv.indexOf('-e') + 1]}(?![\\w$])`).test(corpus);
  let calls = 0;
  const check = async (repo: string, deps: string, text: string, known?: Map<string, boolean>) => {
    const api = { env: { exec: async (argv: string[]) => (calls++, { ok: true, value: { exitCode: has(argv[0] === 'git' ? repo : deps, argv) ? 0 : 1 } }) } };
    return misquotes(api as never, 'r', { index: 1, label: 'nit', text } as never, '', '', {} as never, known);
  };
  const spans = [
    'outer.push({ type: "error", reason: "error", error: message }); outer.end(message)',
    'stream.push({ type: "error", reason: output.stopReason, error: output }); stream.end()',
  ];
  const dep = 'outer.push({ type: "error", reason: "error", error: message }); outer.end(message); stream.push({ type: "error", reason: output.stopReason, error: output }); stream.end()';
  for (const s of spans) {
    assert.deepEqual(await check('', dep, `emits \`${s}\``), []);
    assert.deepEqual(await check('', '', `emits \`${s}\``), claimedNames(s), 'in no source, so invented');
  }
  const asserted = "assert.equal(noticeFor({ status: 'done' }, true), undefined)";
  assert.deepEqual(await check(asserted, '', 'so `noticeFor(outcome, true) === undefined` holds'), []);
  assert.deepEqual(await check('observeGeneration({ messages: [1] })', '', 'pass `{ messages }` through'), []);
  const otlp = 'resp.partialSuccess?.rejectedSpans';
  assert.deepEqual(await check(otlp, '', 'read `ExportTraceServiceResponse.partialSuccess { errorCount, errorMessage }`'), ['errorCount', 'errorMessage']);
  assert.deepEqual(await check('map.keys().next().value', '', 'use `const oldest = map.keys().next().value` then `if (oldest !== undefined)`'), [], 'a new local name is a proposal');
  assert.deepEqual(claimedNames('const oldest = map.keys().next().value'), ['keys', 'next', 'value']);
  const shared = new Map<string, boolean>();
  calls = 0;
  for (const t of ['calls `gone.fetchAll() === 1`', 'again `gone.fetchAll() === 1`']) assert.deepEqual(await check('', '', t, shared), ['fetchAll']);
  assert.equal(calls, 2, 'two searches (repo, deps) for one name across two findings');
});

test('dependencies are searched where the reviewed repo installs them, each search bounded; one that times out does not convict', async () => {
  const argvs: string[][] = [];
  const opts: unknown[] = [];
  const api = (dep: (argv: string[]) => unknown) => ({ env: { exec: async (argv: string[], o: unknown) => (argvs.push(argv), opts.push(o), argv[0] === 'git' ? { ok: true, value: { exitCode: 1 } } : dep(argv)) } });
  const finding = { index: 1, label: 'nit', text: 'it calls `return computeTotals(a)`' } as never;
  assert.deepEqual(await misquotes(api(() => ({ ok: false, error: { code: 'timeout' } })) as never, 'r', finding, '', '', {} as never, new Map(), ['r/node_modules', 'r/web/node_modules']), []);
  assert.deepEqual(argvs.at(-1)!.slice(-2), ['r/node_modules', 'r/web/node_modules']);
  assert.ok(opts.every((o) => (o as { timeout?: number }).timeout! > 0), 'every search has a timeout');
  assert.deepEqual(await misquotes(api(() => ({ ok: true, value: { exitCode: 1 } })) as never, 'r', finding, '', '', {} as never, new Map(), []), ['computeTotals'], 'no dependencies: only the repo is searched');
  assert.equal(argvs.at(-1)![0], 'git');
  assert.ok(!argvs.flat().some((a) => a.includes('v1/node_modules')), 'no layout is hard-coded');
});

test('dependency dirs sit beside every tracked package.json', async () => {
  const api = { env: { exec: async (_argv: string[], o: { onOutput: (t: string) => void }) => (o.onOutput('package.json\nweb/package.json\n'), { ok: true, value: { exitCode: 0 } }) } };
  assert.deepEqual(await dependencyDirs(api as never, 'repo', {} as never), ['repo/node_modules', 'repo/web/node_modules']);
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

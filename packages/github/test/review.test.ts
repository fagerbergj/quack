import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseReview, reviewKind, resolveCitedPath, reviewEvidence, reviewerRubric, validateReview } from '../src/index.ts';
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
  const ci = (s: string) => ({ change: [{ id: 'change', state: { ci: s } }] });
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
  assert.equal(cites('', { finding: [{ id: 'finding-1', state: {}, cited: true }, { id: 'finding-2', state: {}, cited: false }] as never }).met, 0.5);
  assert.equal(cites('', { finding: [] }).met, 1);
});

test('approval_grounded judges an approve on the review alone, and has no unit on any other verdict', async () => {
  const evidence = async (verdict: string) => reviewEvidence()({ snapshot: async () => undefined, conversationId: 'c' } as never, { content: `## Takeaway\nx\n## Verdict\n${verdict}\n## Findings\n` } as never, {} as never);
  const approve = await evidence('approve');
  assert.deepEqual(approve.approve, [{ id: 'approve', state: { review: '## Takeaway\nx\n## Verdict\napprove\n## Findings\n' } }]);
  for (const v of ['request_changes', 'comment']) assert.deepEqual((await evidence(v)).approve, []);
});

test('the rubric scopes match the evidence the reviewer builds', () => {
  const scopes = new Set(reviewerRubric.criteria.flatMap((c) => c.checks.filter((k) => k.question).map((k) => k.scope ?? 'review')));
  assert.deepEqual([...scopes].sort(), ['approve', 'blocking', 'change', 'finding', 'lower', 'review']);
  assert.ok(reviewerRubric.criteria.flatMap((c) => c.checks).some((k) => k.id === 'blocks_without_consequence' && k.weight === -4));
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
  assert.equal(gate('', { finding: [{ id: 'finding-1', state: {}, misquotes: [] }] } as never).met, 1);
  const failed = gate('', { finding: [{ id: 'finding-1', state: {}, misquotes: [] }, { id: 'finding-2', state: {}, misquotes: ["Points", "x"] }] } as never);
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
    return misquotes(api as never, 'r', { index: 1, label: 'nit', path: 'a.ts', text } as never, '', '', {} as never, known ?? new Map(), [{ exts: ['.ts'], dirs: ['r/node_modules'] }]);
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

test('dependencies are searched with their language, each search bounded; one that times out does not convict', async () => {
  const argvs: string[][] = [];
  const opts: unknown[] = [];
  const api = (dep: (argv: string[]) => unknown) => ({ env: { exec: async (argv: string[], o: unknown) => (argvs.push(argv), opts.push(o), argv[0] === 'git' ? { ok: true, value: { exitCode: 1 } } : dep(argv)) } });
  const finding = { index: 1, label: 'nit', path: 'src/a.ts', text: 'it calls `return computeTotals(a)`' } as never;
  const js = [{ exts: ['.ts', '.js'], dirs: ['r/node_modules', 'r/web/node_modules'] }];
  assert.deepEqual(await misquotes(api(() => ({ ok: false, error: { code: 'timeout' } })) as never, 'r', finding, '', '', {} as never, new Map(), js), []);
  assert.deepEqual(argvs.at(-1), ['grep', '-rqsw', '-F', '--include=*.ts', '--include=*.js', '-e', 'computeTotals', 'r/node_modules', 'r/web/node_modules']);
  assert.ok(opts.every((o) => (o as { timeout?: number }).timeout! > 0), 'every search has a timeout');
  assert.deepEqual(await misquotes(api(() => ({ ok: true, value: { exitCode: 1 } })) as never, 'r', finding, '', '', {} as never, new Map(), js), ['computeTotals']);
  argvs.length = 0;
  const goFinding = { index: 1, label: 'nit', path: 'main.go', text: 'it calls `return computeTotals(a)`' } as never;
  assert.deepEqual(await misquotes(api(() => ({ ok: true, value: { exitCode: 1 } })) as never, 'r', goFinding, '', '', {} as never, new Map(), js), [], 'Go was not searchable, so nothing in it is invented');
  assert.equal(argvs.length, 0, 'and nothing is searched');
});

type Sandbox = { tracked: string[]; present?: string[]; goList?: Record<string, string[]>; python?: Record<string, string[]>; repo?: string; corpus?: Record<string, string> };
// A fake sandbox: tracked files, paths that exist, what `go list` and a venv's python print, and the text under each directory.
function sandbox(s: Sandbox) {
  const argvs: string[][] = [];
  const word = (text: string, id: string) => new RegExp(`(?<![\\w$])${id}(?![\\w$])`).test(text);
  const exec = async (argv: string[], o: { onOutput?: (t: string, c: unknown, i: { stream: string }) => void }) => {
    argvs.push(argv);
    const print = (out: string[], exitCode = 0) => (o.onOutput?.(out.map((l) => `${l}\n`).join(''), {}, { stream: 'stdout' }), { ok: true, value: { exitCode } });
    const id = argv[argv.indexOf('-e') + 1];
    if (argv[3] === 'ls-files') return print(s.tracked);
    if (argv[3] === 'grep') return print([], word(s.repo ?? '', id) ? 0 : 1);
    if (argv[0] === 'ls') return print(argv.slice(3).filter((p) => s.present?.includes(p)));
    if (argv[0] === 'env') return s.goList?.[argv[5]] ? print(s.goList[argv[5]]) : print([], 1);
    if (argv[0] === 'grep') return print([], argv.slice(argv.indexOf('-e') + 2).some((d) => word(s.corpus?.[d] ?? '', id)) ? 0 : 1);
    return s.python?.[argv[0]] ? print(s.python[argv[0]]) : print([], 1);
  };
  return { api: { env: { exec } } as never, argvs };
}

test('each language is searchable only when its installed dependencies and stdlib are: node_modules with TypeScript lib, go list offline, a venv', async () => {
  const deps = (s: Sandbox) => dependencyDirs(sandbox(s).api, 'repo', {} as never);
  const tracked = ['package.json', 'web/package.json', 'src/a.ts'];
  assert.deepEqual(await deps({ tracked, present: ['repo/node_modules', 'repo/node_modules/typescript/lib'] }), [{ exts: ['.js', '.mjs', '.cjs', '.jsx', '.ts', '.mts', '.cts', '.tsx'], dirs: ['repo/node_modules'] }]);
  assert.deepEqual(await deps({ tracked, present: ['repo/node_modules'] }), [], 'no TypeScript lib: the JS stdlib is not searchable');
  assert.deepEqual(await deps({ tracked }), [], 'not installed');
  const goList = { 'repo/': ['/go/src/fmt', '/go/src/net', '/go/src/net/http', '/mod/x@v1', '/mod/x@v1', '/abs/repo'] };
  assert.deepEqual(await deps({ tracked: ['go.mod', 'main.go'], goList }), [{ exts: ['.go'], dirs: ['/go/src/fmt', '/go/src/net', '/mod/x@v1', '/abs/repo'] }]);
  assert.deepEqual(await deps({ tracked: ['go.mod', 'tools/go.mod'], goList }), [], 'a module whose packages go cannot list offline');
  const python = { 'repo/.venv/bin/python': ['/usr/lib/python3.14', '/abs/repo/.venv/lib/python3.14/site-packages'] };
  assert.deepEqual(await deps({ tracked: ['pyproject.toml'], present: ['repo/.venv/bin/python'], python }), [{ exts: ['.py', '.pyi'], dirs: python['repo/.venv/bin/python'] }]);
  assert.deepEqual(await deps({ tracked: ['requirements-dev.txt'], python }), [], 'no venv');
  const none = sandbox({ tracked: ['main.c', 'Makefile'] });
  assert.deepEqual(await dependencyDirs(none.api, 'repo', {} as never), []);
  assert.equal(none.argvs.length, 1, 'no manifest: nothing to look for beyond the file list');
});

test('a quoted name is invented only where its language was searched (JS, Go, Python with and without a venv, no manifest)', async () => {
  const check = async (s: Sandbox, path: string, text: string) => {
    const { api } = sandbox(s);
    return misquotes(api, 'repo', { index: 1, label: 'nit', path, text } as never, '', '', {} as never, new Map(), await dependencyDirs(api, 'repo', {} as never));
  };
  const js = { tracked: ['package.json'], present: ['repo/node_modules', 'repo/node_modules/typescript/lib'], corpus: { 'repo/node_modules': 'outer.end(message); findLastIndex(p)' } };
  assert.deepEqual(await check(js, 'src/a.ts', 'calls `return outer.end(message)`'), []);
  assert.deepEqual(await check(js, 'src/a.ts', 'calls `return totals.computeTotals(a)`'), ['computeTotals'], 'a truly invented name is still caught');
  const go = { tracked: ['go.mod'], goList: { 'repo/': ['/go/src/os', '/mod/colorable@v0.1'] }, corpus: { '/go/src/os': 'func LookupEnv(key string); var Stdout', '/mod/colorable@v0.1': 'func NewNonColorable(w io.Writer)' } };
  assert.deepEqual(await check(go, 'color.go', 'reads `if v, ok := os.LookupEnv("NO_COLOR"); ok {`'), []);
  assert.deepEqual(await check(go, 'color.go', 'wraps `w := colorable.NewNonColorable(os.Stdout)`'), []);
  assert.deepEqual(await check(go, 'color.go', 'calls `return c.applyHexPalette(code)`'), ['applyHexPalette']);
  assert.deepEqual(await check({ ...go, goList: {} }, 'color.go', 'calls `return c.applyHexPalette(code)`'), [], 'modules not installed: unknown, not invented');
  const venv = { tracked: ['pyproject.toml'], repo: 'self.raw = raw', present: ['repo/.venv/bin/python'], python: { 'repo/.venv/bin/python': ['/usr/lib/python3', '/site'] }, corpus: { '/usr/lib/python3': 'import os.path; def commonpath(paths):', '/site': 'def is_fp_closed(obj):' } };
  assert.deepEqual(await check(venv, 'src/adapters.py', 'computes `prefix = os.path.commonpath([a, b])`'), []);
  assert.deepEqual(await check(venv, 'src/adapters.py', 'checks `return is_fp_closed(self.raw)`'), []);
  assert.deepEqual(await check(venv, 'src/adapters.py', 'then `return self.adapter.send_with_retries(request)`'), ['adapter', 'send_with_retries']);
  assert.deepEqual(await check({ ...venv, present: [] }, 'src/adapters.py', 'then `return self.adapter.send_with_retries(request)`'), [], 'no venv: unknown');
  assert.deepEqual(await check({ ...js, tracked: ['package.json', 'tools/requirements.txt'] }, 'tools/render.py', 'then `return self.adapter.send_with_retries(request)`'), [], 'JS searchable does not make Python searchable');
  assert.deepEqual(await check({ tracked: ['main.c'] }, 'main.c', 'calls `return totals_compute(a)`'), [], 'no manifest: unknown');
});

test('a failing git diff fails the evidence build instead of scoring the review on no diff', async () => {
  const pr = { owner: 'acme', repo: 'widgets', number: 7, title: 't', body: '', baseRef: 'main', headSha: 'abc', checks: [], linkedIssues: [] };
  const api = (exitCode: number) => ({ conversationId: 'c', snapshot: async () => ({ pr }), env: { exec: async (_argv: string[], o: { onOutput?: (t: string) => void }) => (o?.onOutput?.(exitCode ? 'fatal: bad revision origin/main...HEAD' : ''), { ok: true, value: { exitCode } }) } });
  const sealed = { content: '## Takeaway\nx\n## Verdict\napprove\n## Findings\n' } as never;
  await assert.rejects(reviewEvidence()(api(128) as never, sealed, {} as never), /git diff in repo failed: fatal: bad revision/);
  const ok = await reviewEvidence()(api(0) as never, sealed, {} as never);
  assert.equal((ok.change[0].state as { diff: string }).diff, '(empty diff)', 'an empty diff that git produced is not an unavailable one');
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

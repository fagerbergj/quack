import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { buildEvidence, decisionModelTokens, score, type CheckResult } from '@quack/judge';
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

test('findings must cite lines that exist', () => {
  const cites = code('claims_grounded', 'cites_existing_lines');
  const two = cites('', facts({ ci: '', findings: [{ id: '1', cited: true }, { id: '2', cited: false }] }));
  assert.deepEqual([two.met, two.detail], [0.5, 'no existing path:line cited in finding-2']);
  assert.equal(cites('', facts({ ci: '', findings: [] })).met, 1);
});

// The points each review shape had in play at version 10, whose threshold was 0.8, and the checks that apply to it now.
const SHAPES = {
  'bare approve': { before: 10, applies: ['promises_handled', 'verified_names_evidence'] },
  'comment, no findings': { before: 7, applies: ['promises_handled', 'verified_names_evidence'] },
  'findings, no blocker': { before: 16, applies: ['promises_handled', 'verified_names_evidence', 'matches_cited_code', 'shows_proposed_change'] },
  'approve with findings': { before: 19, applies: ['promises_handled', 'verified_names_evidence', 'matches_cited_code', 'shows_proposed_change'] },
  'blocking findings': { before: 21, applies: ['promises_handled', 'verified_names_evidence', 'matches_cited_code', 'shows_proposed_change', 'anchor_borne_out'] },
};

test('no review shape may fall further short of full marks and still pass than it could at version 10', () => {
  const weights = Object.fromEntries(reviewerRubric.criteria.flatMap((c) => c.checks).map((k) => [k.id, k.weight]));
  for (const [shape, { before, applies }] of Object.entries(SHAPES)) {
    const possible = applies.reduce((n, id) => n + (weights[id] as number), 0);
    assert.ok((1 - reviewerRubric.threshold) * possible <= 0.2 * before + 1e-9, `${shape}: ${(1 - reviewerRubric.threshold) * possible} points of slack, was ${0.2 * before}`);
  }
});


test('a review with one of two findings contradicted by its citation scores no higher than it did at version 13', () => {
  const v13 = { ...reviewerRubric, version: 13, criteria: reviewerRubric.criteria.map((c) => ({ ...c, checks: c.checks.flatMap((k) => (k.id === 'matches_cited_code' ? [{ ...k, miss: undefined }, { id: 'contradicted', label: 'Cited code contradicts the finding', weight: -4 }] : [k])) })) };
  const result = (criterion: string, id: string, weight: number | 'gate', met: number): CheckResult => ({ criterion, id, label: id, weight, met, units: 2, resolvers: {}, notes: [] });
  const base = (matches: number) => [result('claims_grounded', 'cites_existing_lines', 'gate', 1), result('structured_verdict', 'verdict_matches_labels', 'gate', 1), result('catches_real_issues', 'promises_handled', 3, 1), result('claims_grounded', 'verified_names_evidence', 2, 1), result('constructive_actionable', 'shows_proposed_change', 2, 1), result('claims_grounded', 'matches_cited_code', 3, matches)];
  const before = score(v13, [...base(0.5), result('claims_grounded', 'contradicted', -4, 0.5)]);
  const after = score(reviewerRubric, base(0.5));
  assert.equal(before.passed, false);
  assert.ok(after.score <= before.score + 1e-9 && !after.passed, `${after.score} against ${before.score}`);
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

const PROMISE_DIFF = `diff --git a/src/flag.ts b/src/flag.ts
--- a/src/flag.ts
+++ b/src/flag.ts
@@ -1,2 +1,3 @@
+export const verbose = process.argv.includes('--verbose');
diff --git a/docs/cli.md b/docs/cli.md
--- a/docs/cli.md
+++ b/docs/cli.md
@@ -5 +5,2 @@
+The verbose output lists every request.
`;

test("every check is asked of Clef, and every unit the shared review fixtures build fits Clef's window", async () => {
  const maxInputTokens = 8192;
  // promises_handled is still on the judge model; its rework is pending.
  const checks = Object.entries(reviewDeclaration.checks).filter(([id]) => id !== 'promises_handled');
  assert.deepEqual(checks.filter(([, d]) => d.decisionModel === false).map(([id]) => id), []);
  const dir = new URL('../../../test-fixtures/review/', import.meta.url);
  // Every cited file is 400 long lines, so each excerpt is its full 41 lines.
  const file = `${'const value = compute(input, options); '.repeat(3)}\n`.repeat(400);
  const pr = { owner: 'acme', repo: 'widgets', number: 7, title: 'Adds a `--verbose` flag to the CLI', body: 'Sessions now resume where they stopped.', baseRef: 'main', headSha: 'abc', checks: [], linkedIssues: [] };
  const exec = async (_argv: string[], o: { onOutput?: (t: string) => void }) => (o.onOutput?.(PROMISE_DIFF), { ok: true, value: { exitCode: 0 } });
  const api = { conversationId: 'c', snapshot: async () => ({ pr }), env: { exec, readTextFile: async () => ({ ok: true, value: file }) } };
  const reviews = readdirSync(dir).filter((n) => n.endsWith('.md'));
  const asked = new Set<string>();
  for (const name of reviews) {
    const built = await buildEvidence(reviewDeclaration, await reviewInputs()(api as never, { content: readFileSync(new URL(name, dir), 'utf8') } as never, {} as never));
    for (const [id, d] of checks) {
      for (const unit of built.evidence[d.scope]) {
        asked.add(id);
        assert.ok(decisionModelTokens(unit, [{ id, question: d.question }]) <= maxInputTokens, `${name}: ${unit.id}`);
      }
    }
  }
  assert.deepEqual([...asked].sort(), checks.map(([id]) => id).sort(), 'the fixtures give every check a unit');
});

test('matches_cited_code asks about every path:line a finding cites, each excerpt with the finding once per group', async () => {
  // #2445 v2 finding 3 cites rubric.ts:8 and names index.ts:102 in its text; prod showed only the first, so the judge flagged it.
  const files: Record<string, string> = { 'src/rubric.ts': 'a\nb\nwhole?: true\n', 'src/index.ts': 'x\ny\nnoteOf\n' };
  const api = { conversationId: 'c', snapshot: async () => undefined, env: { exec: async () => ({ ok: true, value: { exitCode: 0 } }), readTextFile: async (p: string) => (files[p.slice('repo/'.length)] ? { ok: true, value: files[p.slice('repo/'.length)] } : { ok: false }) } };
  const content = '## Takeaway\nx\n## Verdict\ncomment\n## Findings\n- suggestion: src/rubric.ts:3 — `whole` is unreachable since src/index.ts:3 never sets it (see github.com:443 and src/rubric.ts:3)\n- nit: gone.ts:9 — z\n';
  const built = await buildEvidence(reviewDeclaration, await reviewInputs()(api as never, { content } as never, {} as never));
  assert.deepEqual(built.evidence.citation.map((u) => [u.id, (u.state.excerpt as { cite: string }).cite]), [['citation 1.1', 'src/rubric.ts:3'], ['citation 1.2', 'src/index.ts:3'], ['citation 2.1', 'gone.ts:9']], 'a mention that resolves to no file is dropped');
  assert.match((built.evidence.citation[1].state.excerpt as { code: string }).code, /3: noteOf/);
  assert.equal((built.evidence.citation[2].state.excerpt as { code: string }).code, '(the cited lines do not exist)', "a finding's own citation stays, so a missing line still fails");
  assert.deepEqual(built.groups?.citation, { 'citation 1.1': '1', 'citation 1.2': '1', 'citation 2.1': '2' });
  assert.match(String(built.evidence.citation[0].shared?.finding), /^suggestion: src\/rubric\.ts:3 `whole` is unreachable/);
  assert.equal(reviewDeclaration.checks.contradicted, undefined, 'a contradiction is the plainest excerpt that does not show the claim');
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
  assert.equal(possible, 13);
});

test('an anchor without its space still parses, a cited root file counts, and an unreadable finding line is a problem', () => {
  const r = parseReview('## Findings\n- blocking(defect): src/a.ts:1 — wrong total\n- nit: Dockerfile:12 — pin the base\n- blocker src/b.ts:2 nope\n');
  assert.deepEqual(r.findings.map((f) => [f.label, f.anchor, f.path, f.line]), [['blocking', 'defect', 'src/a.ts', 1], ['nit', undefined, 'Dockerfile', 12]]);
  const prose = parseReview('## Findings\n- question: node:18 is end of life; why not 20?\n- nit: go.mod:3 — bump\n- nit: ./x/y.go:4 — z\n');
  assert.deepEqual(prose.findings.map((f) => [f.path, f.line]), [[undefined, undefined], ['go.mod', 3], ['./x/y.go', 4]]);
  // #2452 v2 cited .gitignore:11, which exists, yet its evidence read "(the cited lines do not exist)".
  const dotfiles = parseReview('## Findings\n- blocking (delivery): .gitignore:11 — marks .quack/ local-only\n- nit: .env.example:3 — z\n');
  assert.deepEqual(dotfiles.findings.map((f) => [f.path, f.line, f.text]), [['.gitignore', 11, 'marks .quack/ local-only'], ['.env.example', 3, 'z']]);
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

const wrap = (findings: string, extra = '## Verified\n- read it\n\n## Notes\n- remember it\n') => `## Takeaway\nTwo things.\n\n## Verdict\ncomment\n\n## Findings\n${findings}\n${extra}`;
const EVIDENCE = '<details><summary>What I checked</summary>\n\n**Verified**\n\n- read it\n\n**Notes**\n\n- remember it\n\n</details>';

test('a review with only inline findings keeps the takeaway, a count line and the collapsed evidence, with no Verdict or Findings sections', () => {
  const text = wrap('- suggestion (design): a.ts:2 — Rename it. The new name says what it holds.\n  ```ts\n  const kept = 1;\n  ```\n- nit: src/a.ts:3 — Trim it.\n- nit: src/a.ts:4 — Drop it.\n');
  const { body, fallbackBody, comments } = splitReview(text, DIFF, ['src/a.ts'], 'repo');
  assert.deepEqual(comments, [
    { path: 'src/a.ts', line: 2, body: '**suggestion (design)**: Rename it.\n\nThe new name says what it holds.\n  ```ts\n  const kept = 1;\n  ```' },
    { path: 'src/a.ts', line: 3, body: '**nit**: Trim it.' },
    { path: 'src/a.ts', line: 4, body: '**nit**: Drop it.' },
  ]);
  assert.equal(body, `Two things.\n\n1 suggestion, 2 nits inline.\n\n${EVIDENCE}`);
  assert.doesNotMatch(body, /## |Posted inline|…/);
  assert.match(fallbackBody, /Not anchored to a diff line:\n- suggestion \(design\): a\.ts:2 — Rename it\./, 'when GitHub refuses the inline comments every finding is in the body');
});

test('a finding GitHub cannot anchor is listed once in full in the body, and a long inline sentence is never truncated', () => {
  const long = `Rename ${'the thing that holds the value '.repeat(6)}so it reads well.`;
  const text = wrap(`- blocking (defect): src/a.ts:90 — Off the diff.\n  more detail\n- nit: src/a.ts:2 — ${long} Then more.\n`);
  const { body, comments } = splitReview(text, DIFF, ['src/a.ts'], 'repo');
  assert.equal(comments[0].body, `**nit**: ${long}\n\nThen more.`);
  assert.equal(body, `Two things.\n\n1 nit inline.\n\nNot anchored to a diff line:\n- blocking (defect): src/a.ts:90 — Off the diff.\n  more detail\n\n${EVIDENCE}`);
});

test('empty Verified and Notes leave no section and no empty details block', () => {
  const none = splitReview(wrap('- nit: src/a.ts:2 — Trim it.\n', '## Verified\n\n## Notes\n'), DIFF, ['src/a.ts'], 'repo');
  assert.equal(none.body, 'Two things.\n\n1 nit inline.');
  const one = splitReview(wrap('- nit: src/a.ts:2 — Trim it.\n', '## Verified\n- read it\n\n## Notes\n\n'), DIFF, ['src/a.ts'], 'repo');
  assert.equal(one.body, 'Two things.\n\n1 nit inline.\n\n<details><summary>What I checked</summary>\n\n**Verified**\n\n- read it\n\n</details>');
});

test('an approve with no findings is the takeaway and the collapsed evidence', () => {
  const text = `## Takeaway\nClean change; approve.\n\n## Verdict\napprove\n\n## Findings\n\n## Verified\n- read it\n\n## Notes\n- remember it\n`;
  const { body, comments } = splitReview(text, DIFF, ['src/a.ts'], 'repo');
  assert.deepEqual(comments, []);
  assert.equal(body, `Clean change; approve.\n\n${EVIDENCE}`);
});

test('a re-review does not repeat a finding already posted on the same line, and says what changed since', () => {
  const first = wrap('- nit: src/a.ts:2 — Trim it.\n- nit: src/a.ts:3 — Drop it.\n');
  const second = wrap('- nit: src/a.ts:2 — Trim it.\n- suggestion: src/a.ts:4 — New idea.\n');
  const { body, comments } = splitReview(second, DIFF, ['src/a.ts'], 'repo', first);
  assert.deepEqual(comments.map((c) => c.body), ['**suggestion**: New idea.']);
  assert.equal(body, `Two things.\n\n1 suggestion inline. Since the last review: 1 no longer raised, 1 still open in their threads.\n\n${EVIDENCE}`);
});

test('the review kind reads its outcome from the verdict, as the review is posted', () => {
  assert.equal(reviewKind.outcome?.(review), 'request_changes');
  assert.equal(reviewKind.outcome?.('# Review\nfine\n'), undefined);
});

test('an excerpt is sent once when the full path a finding cites and the short path it mentions resolve to the same file', async () => {
  const full = 'packages/github/src/review.ts';
  const exec = async (_argv: string[], o: { onOutput?: (t: string) => void }) => (o.onOutput?.(`${full}\n`), { ok: true, value: { exitCode: 0 } });
  const readTextFile = async (p: string) => (p === `repo/${full}` ? { ok: true, value: 'a\nb\nc\nd\ne\n' } : { ok: false });
  const api = { conversationId: 'c', snapshot: async () => undefined, env: { exec, readTextFile } };
  const content = `## Takeaway\nx\n## Verdict\ncomment\n## Findings\n- nit: ${full}:5 — same line as review.ts:5\n`;
  const built = await buildEvidence(reviewDeclaration, await reviewInputs()(api as never, { content } as never, {} as never));
  assert.deepEqual(built.evidence.citation.map((u) => (u.state.excerpt as { cite: string }).cite), [`${full}:5`]);
});

test('a Verified item names its source when it shows a command, file:line, CI check, commit or URL; an absence claim needs its search', async () => {
  // The 77 Verified items of the replay set (14 recorded reviews and a re-review), each label checked by hand; a bare
  // "ran"/"read" names no source for code, so those items are asked of Clef.
  const items: Array<{ item: string; backed: boolean; text: string }> = JSON.parse(readFileSync(new URL('./fixtures/verified-items.json', import.meta.url), 'utf8'));
  assert.equal(items.length, 77);
  assert.deepEqual(items.filter((it) => (namedSources(it.text) > 0) !== it.backed).map((it) => it.item), []);
  const ran = ['Ran `cargo test -p judge --release`: 41 pass.', 'Ran `FOO=1 make check` clean.', "`pnpm -r exec 'tsc --noEmit'` is clean."];
  const read = ['`const a = 2;` is unchanged.', 'Read `onEvent` and its callers.', 'The logic is correct and the tests pass.', 'Confirmed `legacy` is referenced nowhere.'];
  assert.deepEqual([...ran, ...read].map((t) => namedSources(t) > 0), [true, true, true, false, true, false, false], 'a command line or the code read counts; a statement, a bare claim or an unsearched absence does not');
  const loose = ['the retry model in fixtures.ts mirrors the service.'];
  assert.deepEqual(loose.map((t) => namedSources(t) > 0), [true], 'a file without a line counts');
  assert.deepEqual(['Ran the targeted suites: 46/46 pass.', 'Read the full 62-file diff.', 'Nobody ran the tests for this path.', 'I have not read the diff, only the summary.', 'did not read the vendored files'].map((t) => namedSources(t) > 0), [false, false, false, false, false], 'a bare verb names no source, so Clef reads it, negation included');
  // #2470 v2 Verified item 9 claimed a result and named nothing; the judge flagged it, and it is still asked.
  assert.equal(namedSources('Quote pairing itself is sound: on a line with two quoted phrases both read as mentions, and a real claim after a closed quote on the same line is still checked.'), 0);
  const checks = ['build', 'e2e'];
  assert.deepEqual(['`e2e` passed on this head.', 'build=success.', 'The build passed.'].map((t) => namedSources(t, checks) > 0), [true, true, false], "the PR's own checks count when cited, not as bare words");
  const absent = ['Nothing imports `legacy` any more.', 'No callers of `x` are left.', '`rg legacy` finds nothing; no imports remain.', 'Searched the tree: nothing references `x`.'];
  assert.deepEqual(absent.map((t) => namedSources(t) > 0), [false, false, true, true], 'an absence claim is backed only by its search');
  const built = buildEvidence({ scopes: { verified: reviewDeclaration.scopes.verified }, checks: {} }, { review: '## Verified\n- CI `check` is green on this head.\n- Confirmed `x` is referenced nowhere.\n' });
  assert.deepEqual(built.evidence.verified.map((u) => [u.id, u.related]), [['verified item-1', { sources: 2 }], ['verified item-2', { sources: 0 }]]);
  assert.deepEqual(reviewDeclaration.checks.verified_names_evidence.aggregate, { name: 'share', by: 'either' }, 'a named source settles the item; the rest go to Clef');
});


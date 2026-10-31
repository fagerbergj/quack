import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseReview, reviewerRubric, validateReview } from '../src/index.ts';
import { quotedCode } from '../src/review.ts';

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
});

test('blocking findings must name an anchor, and findings must cite lines that exist', () => {
  const anchor = code('severity_grounded', 'blocking_names_anchor');
  assert.equal(anchor(review, {}).met, 1);
  assert.match(anchor(review.replace('blocking (defect)', 'blocking'), {}).detail!, /finding-1/);
  const cites = code('claims_grounded', 'cites_existing_lines');
  assert.equal(cites('', { finding: [{ id: 'finding-1', state: {}, cited: true }, { id: 'finding-2', state: {}, cited: false }] as never }).met, 0.5);
  assert.equal(cites('', { finding: [] }).met, 1);
});

test('the rubric scopes match the evidence the reviewer builds', () => {
  const scopes = new Set(reviewerRubric.criteria.flatMap((c) => c.checks.filter((k) => k.question).map((k) => k.scope ?? 'review')));
  assert.deepEqual([...scopes].sort(), ['blocking', 'change', 'finding', 'lower', 'review']);
  assert.ok(reviewerRubric.criteria.flatMap((c) => c.checks).some((k) => k.id === 'blocks_without_consequence' && k.weight === -4));
  const possible = reviewerRubric.criteria.flatMap((c) => c.checks).reduce((n, k) => n + (typeof k.weight === 'number' && k.weight > 0 ? k.weight : 0), 0);
  assert.equal(possible, 27);
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
  const failed = gate('', { finding: [{ id: 'finding-1', state: {}, misquotes: [] }, { id: 'finding-2', state: {}, misquotes: ['Points: 0', 'x := y'] }] } as never);
  assert.equal(failed.met, 0.5);
  assert.match(failed.detail!, /finding-2 quotes code that is not in the repository: `Points: 0`, `x := y`/);
  assert.match(failed.detail!, /command output or logs go in a fenced block/);
  assert.deepEqual(quotedCode('it fails:\n```text\npanic: runtime error: x == nil {\n```'), [], 'quoted output in a fence is not checked');
});

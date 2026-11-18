import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { buildEvidence } from '@quack/judge';
import { derivations, recomputes } from '../src/claims.ts';
import { inputsOf, reportDeclaration, researchRubric } from '../src/report.ts';

const REPORT = [
  '## Summary',
  'The SR24UBFFD gives 1,066.8 mm of mounting height [s1].',
  '## Assumptions',
  '- 1U = 44.45 mm (EIA-310).',
  '## Findings',
  '- The cabinet is 24U. [s1]',
  '- The Define 7 XL is 566 mm tall. [s1]',
  '## Verdict',
  '- Mounting height: 24U × 44.45 mm = 1,066.8 mm.',
  '- Above the tower: 1,066.8 − 566 = 500.8 mm = 11.27 U.',
  '- All three stack to 566 + 460 + 250 = 1,286 mm.',
  '- Marketek lists it at $1,512.61.',
].join('\n');

const code = (id: string, report: string, request = '') => researchRubric.criteria.flatMap((c) => c.checks).find((k) => k.id === id)!.code!(report, { request: [{ id: 'request', state: { request } }] });
const traced = (summary: string, findings: string, extra = '', request = '') => code('derived_inputs_traced', `## Summary\n${summary}\n## Assumptions\nNone.\n## Findings\n${findings}\n${extra}`, request);

test('derivations: equations, chains, parenthesised expressions and conversions are recomputed; ranges, dimension lists and size classes are not derivations', () => {
  const shown = (text: string) => derivations(text).map((d) => [d.inputs, d.shown, recomputes(d)]);
  assert.deepEqual(shown('Above it: 1,066.8 − 566 = **500.8 mm = 11.27 U** free'), [[['1066.8', '566'], '500.8', true], [['500.8', '44.45'], '11.27', true]]);
  assert.deepEqual(shown('1,066.8 mm (24 × 44.45 mm) of mounting'), [[['24', '44.45'], '1066.8', true]]);
  assert.deepEqual(shown('23.6" (≈600 mm) wide'), [[['23.6'], '600', true]], 'within one in the last shown digit');
  assert.deepEqual(shown('~82 cm (32.5")'), [[['32.5'], '82', true]], 'either side may be the input');
  assert.deepEqual(shown('1,000 lb (≈450 kg)'), [[['1000'], '450', false]], '453.6 is not 450 at the precision shown');
  assert.deepEqual(shown('566 + 460 + 250 = 1,286 mm'), [[['566', '460', '250'], '1286', false]]);
  for (const none of ['~1,400 mm ≈ 31–32U', 'a 12U (634 mm) cabinet', '46.8 × 23.6 × 38.5 in = 1,189 × 600 × 978 mm', '38.5 in (97.79 × 59.95 cm)', '566 mm (case height) ÷ 44.45 = 12.7U', 'needs 2–6 U'])
    assert.deepEqual(shown(none), [], none);
});

test('derived_arithmetic recomputes what the sections outside Findings and Assumptions show, naming the figure that does not recompute', () => {
  const r = code('derived_arithmetic', REPORT);
  assert.equal(r.met, 0.75);
  assert.equal(r.detail, '"566 + 460 + 250 = 1,286 mm" shows 1286; its inputs give 1276.0');
  assert.deepEqual(code('derived_arithmetic', REPORT.replace('## Verdict', '## Findings (cont.)').replace('## Findings\n', '## Findings\n- 566 + 460 + 250 = 1,286 mm [s1]\n')).met, 0.75, 'arithmetic inside Findings is the source checks\' to judge');
});

test('derived_inputs_traced: a figure outside Findings is a cited finding\'s, one Assumptions lists, or recomputed from those; the rest are named', () => {
  const r = code('derived_inputs_traced', REPORT);
  assert.equal(r.met, 9 / 13);
  assert.match(r.detail!, /^460 in "All three stack to 566 \+ 460 \+ 250 = 1,286 mm\.…"; 250 in .*; 1286 in .*; 1512\.61 in "Marketek lists it at \$1,512\.61\.…" cite no finding and are not listed under Assumptions$/);
  assert.equal(code('derived_inputs_traced', REPORT.replace('- 1U = 44.45 mm (EIA-310).', 'None.')).met, 3 / 13, 'without the listed constant, nothing computed from it traces');
});

test('the derived checks are code only, at weight 0: no derived sentence becomes a model question', async () => {
  const ev = (await buildEvidence(reportDeclaration, inputsOf(REPORT, undefined, new Map(), ['s1']))).evidence;
  assert.equal(ev.derived, undefined);
  assert.deepEqual(researchRubric.criteria.find((c) => c.id === 'derived_grounded')!.checks.map((k) => [k.id, k.weight, !!k.code]), [['derived_arithmetic', 0, true], ['derived_inputs_traced', 0, true]]);
});

test('derived_inputs_traced: each false-positive class of the recorded reports traces at its cause', () => {
  assert.equal(traced('Kestrel runs 38 buses.', '**Fleet.** Kestrel operates 38 battery-electric buses [s1].').met, 1, 'a cited paragraph in Findings is a finding');
  assert.equal(traced('Kestrel runs 38 buses.', '- x [s1]', '## Findings — Taunton\n- Kestrel operates 38 buses [s1].\n').met, 0, 'only the exact heading is Findings (research.report v6), so a variant derives');
  assert.equal(traced('x', '- x [s1]', '## Sources\n- [s1] Kestrel Transit fleet report, March 2026, p. 4.\n').met, 1, 'a line that lists a source derives nothing');
  assert.equal(traced('The guides share (1) a welcome, (2) setup steps and (3) a review policy; Option 2 — a filtered intake.', '- x [s1]').met, 1, 'list numbers and labels are not figures');
  assert.equal(traced('The kit has (2) keys and (4) casters.', '- x [s1]').met, 0, 'counts in parentheses that are not a run 1, 2, 3 stay figures');
  assert.equal(traced('It enters maintenance on October 20, 2027, as planned in 2027.', '- Maintenance starts 2027-10-20. [s1]').met, 1, 'one date written two ways, and its year, are one figure');
  assert.equal(traced('Leave room for a 0U PDU.', '- The rails take a 0U PDU. [s1]').met, 1, 'a number in a name the findings use traces');
  const asked = traced('The tower is 566 mm tall; the APC is $3,045.', '- x [s1]', '', 'My tower is 566 mm tall.');
  assert.equal(asked.detail, '3045 in "The tower is 566 mm tall; the APC is $3,045.…" cite no finding and are not listed under Assumptions. 566 in "The tower is 566 mm tall; the APC is $3,045.…" come from the request, not from a finding or assumption');
  assert.equal(asked.met, 0, 'a figure from the request is still untraced, as the owner decided');
});

test('derived_inputs_traced does not trace the labels the sentence splitter cuts off a line', () => {
  const report = '## Summary\nThe Tripp Lite suits a dusty shop. [s1]\n## Assumptions\nNone.\n## Findings\n- Eight fans pull air through two filters. [s1]\n## Analysis\n- **3. Ventilation.** The door fans pull air in through the filters [s1].\n- **2.**\n';
  assert.equal(code('derived_inputs_traced', report).met, 1, 'the "2" of a list label is no figure to trace');
});

test('derived_inputs_traced: a unit hyphened onto its number in a finding ("10-ft.") is that figure', () => {
  assert.equal(traced('The fans have their own 10 ft cord.', '- The package includes a C13 power cord, 10-ft. (3.05 m) [s1]').met, 1);
});

test('derived_inputs_traced: a count a finding writes as a word ("Eight fans") is the figure a derived sentence writes in numerals', () => {
  assert.equal(traced('Run those 8 door fans as intake.', '- Eight high-performance fans are built into the door. [s1]').met, 1);
});

test('derived_inputs_traced: "0U" names a zero-U PDU, not a figure to trace', () => {
  assert.equal(traced('Leave room for a vertical 0U PDU on a rail.', '- The cabinet has 4 rails. [s1]').met, 1);
});

test('derivations: an operand may carry a label, and a traced sum of labelled operands traces its result', () => {
  assert.deepEqual(derivations('Your tower stack: Define 7 XL 566 mm + Define 5 ~460 mm + mATX ~200 mm = **~1,226 mm** for the three cases alone').map((d) => [d.inputs, d.shown, recomputes(d)]), [[['566', '460', '200'], '1226', true]]);
  const report = '## Summary\nx\n## Assumptions\n- The mATX case is about 200 mm tall.\n## Findings\n- The Define 7 XL is 566 mm tall and the Define 5 is 460 mm tall. [s1]\n## Analysis\n- Your tower stack: Define 7 XL 566 mm + Define 5 ~460 mm + mATX ~200 mm = ~1,226 mm.\n- The three cases need ~1,226 mm.\n';
  assert.deepEqual([code('derived_arithmetic', report).met, code('derived_inputs_traced', report).met], [1, 1]);
});

test('derivations: an "≈" before an operand marks it approximate, so the chain is still the sum it shows', () => {
  const shown = (text: string) => derivations(text).map((d) => [d.inputs, d.shown, recomputes(d)]);
  assert.deepEqual(shown('The stack is 566 + ≈460 = 1,026 mm tall.'), [[['566', '460'], '1026', true]]);
  assert.deepEqual(shown('The stack is 566 + ≈460 ≈ 1,026 mm tall.'), [[['566', '460'], '1026', true]]);
  assert.deepEqual(shown('The stack is ≈566 + 460 = 1,026 mm tall.'), [[['566', '460'], '1026', true]]);
  assert.deepEqual(shown('Mounting: 24U × ≈44.45 mm = 1,066.8 mm.'), [[['24', '44.45'], '1066.8', true]]);
});

// Reports sealed on prod, and chains code cannot read; no shape may throw, since a throwing code check rejects the whole judge run.
const SEALED = readdirSync(new URL('./fixtures/reports/', import.meta.url)).filter((f) => f.endsWith('.md')).map((f) => readFileSync(new URL(`./fixtures/reports/${f}`, import.meta.url), 'utf8'));
const MALFORMED = ['= 1,026 mm', '566 + = 1,026', '≈ ≈ 5', '566 + ≈ = 3', '1 = = 2', '(≈)', '12 (× 4)', '5 mm (≈ cm)', '+ 4 = 4', '7 ≈', '≈≈≈', '3 − ≈ − 2 = 1', '$ + $ = $'];

test('derivations and both derived checks never throw, on every line of the sealed reports and on malformed chains', () => {
  const lines = [...SEALED.flatMap((r) => r.split('\n')), ...MALFORMED];
  assert.ok(lines.length > 100);
  for (const line of lines) {
    assert.doesNotThrow(() => derivations(line), line);
    const report = `## Summary\n${line}\n## Assumptions\nNone.\n## Findings\n- ${line} [s1]\n## Verdict\n- ${line}\n`;
    assert.doesNotThrow(() => [code('derived_arithmetic', report), code('derived_inputs_traced', report)], line);
  }
  for (const r of SEALED) assert.doesNotThrow(() => [code('derived_arithmetic', r), code('derived_inputs_traced', r)]);
});

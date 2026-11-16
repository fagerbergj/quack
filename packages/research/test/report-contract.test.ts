import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { buildEvidence } from '@quack/judge';
import { citedIds, findingItems, inputsOf, parseReport, reportDeclaration, summarizeReport, validateReport } from '../src/report.ts';

const CONTRACT = { key: 'contracts:e0c7a5bc-36ee-463a-85a9-c296403f5e1e', version: 6 };
const read = (name: string) => readFileSync(new URL(`./fixtures/${name}`, import.meta.url), 'utf8');
const schema = JSON.parse(read('report-contract-v6.json'));
const accepts = new Ajv2020({ strict: false }).compile(schema);
// Sealed prod reports; stored-town-sections.md is synthetic, in the shape of a stored report the seal now refuses.
const SEALED = ['node-lts.md', 'aip-158-page-size.md', 'tautological-tests.md', 'parallel-implementations.md'];

test(`the checked-in schema is ${CONTRACT.key} v${CONTRACT.version}, and every sealed fixture report seals and parses to it`, () => {
  assert.equal(schema.title, 'research.report, parsed');
  for (const f of SEALED) {
    const md = read(`reports/${f}`);
    assert.deepEqual(validateReport(md), [], f);
    assert.equal(accepts(parseReport(md)), true, `${f}: ${JSON.stringify(accepts.errors)}`);
  }
});

test("the contract's examples are the parse of the Markdown they describe", () => {
  for (const ex of schema.examples) {
    const md = Object.entries(ex.sections as Record<string, string>).map(([k, v]) => `## ${k[0].toUpperCase()}${k.slice(1)}\n${v}\n`).join('\n');
    assert.deepEqual(parseReport(md), ex);
  }
});

const report = (summary = '## Summary', findings = '## Findings', extra = '') => `${summary}\nDucks fly [s1].\n\n## Assumptions\nNone.\n\n${findings}\n- Ducks are birds. [s1]\n${extra}`;
const refused = (name: string, ...near: string[]) => `missing a "## ${name}" section: the heading must read exactly "## ${name}", once, and ${near.map((h) => `"${h}"`).join(', ')} ${near.length > 1 ? 'do' : 'does'} not count`;

test('a required section is its whole heading, outside code fences, once; the refusal names the heading and the form', () => {
  assert.deepEqual(validateReport(report('## Summary:')), [refused('Summary', '## Summary:')]);
  assert.deepEqual(validateReport(report('## Summary of results')), [refused('Summary', '## Summary of results')]);
  assert.deepEqual(validateReport(report('##Summary')), [refused('Summary', '##Summary')]);
  assert.deepEqual(validateReport(report('## Summary', '## Findings — Taunton')), [refused('Findings', '## Findings — Taunton')]);
  assert.deepEqual(validateReport(report('## Summary', '## Findings — Taunton', '\n## Findings — Raynham\n- More. [s2]\n')), [refused('Findings', '## Findings — Taunton', '## Findings — Raynham')]);
  assert.deepEqual(validateReport(report('## Summary', '## Findings', '\n## Findings\n- More. [s2]\n')), ['"## Findings" appears 2 times; a report has one "## Findings" section, so merge them']);
  assert.deepEqual(validateReport('```\n## Summary\n## Assumptions\n## Findings\n```\n[s1]'), ['missing a "## Summary" section', 'missing a "## Assumptions" section', 'missing a "## Findings" section']);
  assert.deepEqual(validateReport(report('## SUMMARY  ', '## findings')), [], 'case and surrounding spaces do not matter');
  assert.deepEqual(validateReport(report('## Summary', '## Findings', '\n## Analysis\n- Ducks migrate. [s2]\n\n## Verdict\nGo.\n')), [], 'extra sections are allowed');
});

test('extra sections are kept in the parse, and their bullets are not findings', () => {
  const p = parseReport(report('## Summary', '## Findings', '\n## Analysis\n- Ducks migrate. [s2]\n\n## Verdict\nGo.\n'));
  assert.deepEqual(p.sections, { summary: 'Ducks fly [s1].', assumptions: 'None.', findings: '- Ducks are birds. [s1]', analysis: '- Ducks migrate. [s2]', verdict: 'Go.' });
  assert.deepEqual(p.findings, [{ first: '- Ducks are birds. [s1]', text: '- Ducks are birds. [s1]' }]);
  assert.deepEqual(p.citedIds, ['s1', 's2']);
});

test('findings come from the one "## Findings" section, not one that only starts with the word', () => {
  assert.deepEqual(findingItems('## Findings\n- a [s1]\n## Findings and caveats\n- b [s2]\n'), [{ first: '- a [s1]', text: '- a [s1]' }]);
  assert.deepEqual(findingItems('## Findings and caveats\n- a [s1]'), []);
});

test('a mark in a code fence or in inline code cites nothing', () => {
  assert.deepEqual(citedIds('```\n[s5]\n```\n`[s7]` and ``x [s8]``\n~~~\n[s9]\n~~~\n[s1]'), ['s1']);
  assert.deepEqual(validateReport('## Summary\nDucks fly `[s1]`.\n## Assumptions\nNone.\n## Findings\n```\n- Ducks are birds. [s1]\n```\n'), ['no source is cited; cite fetched sources as [s1], [s2]']);
  const example = findingItems(read('reports/tautological-tests.md')).find((f) => f.text.includes('# [s20]'))!;
  assert.deepEqual(citedIds(example.text), ['s16'], 'the marks in its indented code block are the code\'s, not the finding\'s');
});

test("the stored summary and the judge's Summary scope read the same section, a fenced heading inside it included", async () => {
  const md = '# Buses\n\n## Summary\nKestrel runs 38 electric buses [s1].\n```\n## Notes\n```\nIt plans 20 more.\n\n## Assumptions\nNone.\n\n## Findings\n- Kestrel runs 38 electric buses. [s1]\n';
  const sentences = async (text: string) => (await buildEvidence(reportDeclaration, inputsOf(text, undefined, new Map(), []))).evidence.summary.map((u) => u.state.sentence);
  assert.equal(summarizeReport(md), 'Kestrel runs 38 electric buses.');
  assert.deepEqual(await sentences(md), ['Kestrel runs 38 electric buses.', 'It plans 20 more.']);
  const colon = md.replace('## Summary', '## Summary:');
  assert.equal(summarizeReport(colon), 'Buses', 'no Summary section, so the first heading');
  assert.deepEqual(await sentences(colon), []);
});

test('a stored report the seal now refuses still reads: its Summary line, no findings, and every bullet outside the Summary as its context', async () => {
  const md = read('reports/stored-town-sections.md');
  assert.deepEqual(validateReport(md), [refused('Findings', '## Findings — Eastharbour', '## Findings — Westmere / Lowford', '## Findings — Northgate')]);
  assert.equal(summarizeReport(md), 'Five yards around Eastharbour and Westmere build small wooden boats to order; two take a single dinghy commission.');
  assert.deepEqual(findingItems(md), []);
  assert.deepEqual(citedIds(md), ['s1', 's3', 's2']);
  assert.equal(accepts(parseReport(md)), false, 'its parse lacks the findings section, as the seal says');
  const ev = (await buildEvidence(reportDeclaration, inputsOf(md, undefined, new Map([['s1', 'x']]), ['s1', 's2', 's3']))).evidence;
  assert.deepEqual(ev.claim, []);
  assert.match(String((ev.summary[0].shared as { findings: string }).findings), /Pell & Daughters[\s\S]*Harker Boats[\s\S]*Ask Lowford first/);
});

const F = (body: string) => `## Findings\n${body}`;
// Today's findingItems outputs, pinned: the finding rules are unchanged by the heading rule.
const PINNED: Array<[string, string, string[][]]> = [
  ['topBullets', F('- a [s1]\n- b [s2]'), [['- a [s1]', '- a [s1]'], ['- b [s2]', '- b [s2]']]],
  ['star', F('* a [s1]\n* b [s2]'), [['* a [s1]', '* a [s1]'], ['* b [s2]', '* b [s2]']]],
  ['numbered', F('1. a [s1]\n2. b [s2]'), [['1. a [s1]', '1. a [s1]'], ['2. b [s2]', '2. b [s2]']]],
  ['subbullets', F('- a [s1]\n  - sub [s2]\n- b'), [['- a [s1]', '- a [s1]'], ['- sub [s2]', '- a [s1]\n  - sub [s2]'], ['- b', '- b']]],
  ['leadIn', F('- Cabins:\n  - A [s1]\n  - B [s2]'), [['- A [s1]', '  - A [s1]'], ['- B [s2]', '  - B [s2]']]],
  ['leadInCited', F('- Cabins [s9]:\n  - A [s1]\n  - B [s2]'), [['- A [s1]', '- Cabins [s9]:\n  - A [s1]'], ['- B [s2]', '- Cabins [s9]:\n  - B [s2]']]],
  ['leadInNoKids', F('- Cabins:\n- B [s2]'), [['- Cabins:', '- Cabins:'], ['- B [s2]', '- B [s2]']]],
  ['leadInLoneNoKids', F('- Cabins differ:'), [['- Cabins differ:', '- Cabins differ:']]],
  ['leadInBoldColon', F('- **Cabins:** [s9]\n  - A [s1]'), [['- A [s1]', '- **Cabins:** [s9]\n  - A [s1]']]],
  ['leadInNotColon', F('- Cabins\n  - A [s1]'), [['- Cabins', '- Cabins'], ['- A [s1]', '  - A [s1]']]],
  ['paraNotBullet', F('Cabin A sleeps two [s1].\n\n- b [s2]'), [['- b [s2]', 'Cabin A sleeps two [s1].\n- b [s2]']]],
  ['paraOnly', F('Cabin A sleeps two [s1].'), []],
  ['paraAboveListCited', F('Cabin facts [s3]:\n- A [s1]\n- B [s2]'), [['- A [s1]', 'Cabin facts [s3]:\n- A [s1]'], ['- B [s2]', 'Cabin facts [s3]:\n- B [s2]']]],
  ['paraAboveListUncited', F('Cabin facts:\n- A [s1]'), [['- A [s1]', '- A [s1]']]],
  ['continuation', F('- a starts\n  continues [s1]\n- b'), [['- a starts', '- a starts\n  continues [s1]'], ['- b', '- b']]],
  ['twoSections', '## Findings\n- a [s1]\n## Findings\n- b [s2]', [['- a [s1]', '- a [s1]'], ['- b [s2]', '- b [s2]']]],
  ['fencedBullet', F('```\n- not a finding [s1]\n```\n- b [s2]'), [['- b [s2]', '- b [s2]']]],
  ['tableBeforeBullets', F('| a | b |\n- c [s1]'), [['- c [s1]', '- c [s1]']]],
  ['checkbox', F('- [x] a [s1]'), [['- [x] a [s1]', '- [x] a [s1]']]],
  ['subOnlyIndent', F('- a:\n    - deep [s1]\n      - deeper [s2]'), [['- deep [s1]', '    - deep [s1]'], ['- deeper [s2]', '    - deep [s1]\n      - deeper [s2]']]],
  ['extraSectionBullets', '## Findings\n- a [s1]\n## Analysis\n- extra [s2]', [['- a [s1]', '- a [s1]']]],
  ['h3Inside', F('### Group\n- a [s1]'), [['- a [s1]', '- a [s1]']]],
  ['preambleBullets', '- pre [s1]\n## Findings\n- a [s1]', [['- a [s1]', '- a [s1]']]],
];

test('every finding rule gives what it gave before the heading rule', () => {
  for (const [name, md, want] of PINNED) assert.deepEqual(findingItems(md).map((f) => [f.first, f.text]), want, name);
});

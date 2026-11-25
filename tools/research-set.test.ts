import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildEvidence, codeResults } from '@quack/judge';
import { reportDeclaration, researchRubric } from '@quack/research';
import { researchCases } from './research/cases.ts';

test('the research set has its required mix, every case passes the link gate, and each flaw sits in a unit of the check it targets', () => {
  const cases = researchCases();
  assert.ok(cases.length >= 12 && cases.filter((c) => c.good).length >= 4);
  for (const id of Object.keys(reportDeclaration.checks)) assert.ok(cases.filter((c) => c.targets.includes(id as never)).length >= 2, id);
  for (const c of cases) {
    assert.equal(c.good, c.targets.length === 0, c.case);
    const { evidence } = buildEvidence(reportDeclaration, c.inputs);
    assert.deepEqual(codeResults(researchRubric, c.content, evidence).filter((r) => r.weight === 'gate' && Number(r.met ?? 1) < 1).map((r) => r.id), [], c.case);
    for (const t of c.targets) assert.ok(evidence[t].length > 0, `${c.case}: no ${t} unit to catch its flaw`);
  }
});

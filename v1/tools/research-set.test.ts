import assert from 'node:assert/strict';
import { test } from 'node:test';
import { codeResults } from '@quack/judge';
import { researchRubric } from '@quack/research';
import { researchCases } from './research/cases.ts';

test('the research set has its required mix and every case passes the code gates, so only the model checks can catch its flaw', () => {
  const cases = researchCases();
  assert.ok(cases.length >= 12 && cases.filter((c) => c.good).length >= 4);
  for (const word of ['unsupported', 'contradicted', 'unanswered', 'wrong-year']) assert.ok(cases.filter((c) => c.case.startsWith(word)).length >= 2, word);
  for (const c of cases) {
    assert.equal(c.good, c.targets.length === 0, c.case);
    assert.deepEqual(codeResults(researchRubric, c.content, c.evidence).filter((r) => (r.met ?? 1) < 1).map((r) => r.id), [], c.case);
  }
});

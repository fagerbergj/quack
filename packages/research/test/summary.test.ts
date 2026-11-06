import assert from 'node:assert/strict';
import { test } from 'node:test';
import { summarizeReport } from '../src/report.ts';

test('a report summarizes as its Summary line without markdown or source marks', () => {
  assert.equal(summarizeReport('# T\n\n## Summary\n\n**DFlash** is faster [s1].\nMore.\n\n## Findings\n- x [s1]\n'), 'DFlash is faster.');
});

test('without a Summary section it falls back to the first heading', () => {
  assert.equal(summarizeReport('# Speculative decoding\n\n## Findings\n- x\n'), 'Speculative decoding');
});

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { cardsKind } from '../src/lineup/card.ts';

const sum = (c: object) => cardsKind.summarize!(JSON.stringify(c));

test('a lineup card summarizes as the projections, other cards as their own summary', () => {
  assert.equal(sum({ kind: 'lineup', my_proj: 112.4, opp_proj: 98.1, summary: 'Start Allen' }), '112.4 vs 98.1 projected');
  assert.equal(sum({ kind: 'lineup', my_proj: 112.4, opp_proj: null, summary: 'Start Allen' }), 'Start Allen');
  assert.equal(sum({ kind: 'waivers', summary: 'Claim two backs', candidates: [] }), 'Claim two backs');
});

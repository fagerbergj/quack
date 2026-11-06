import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { assertSealFits, EVENT_MAX_BYTES, gated, type SealedArtifact } from '../src/index.ts';

const version = (content: string): SealedArtifact => ({ kind: 'review', version: 1, hash: 'h', content, sealedAt: 0, verdict: null });

test('a seal whose lifecycle event would pass the cap is refused with a reason the agent can act on', () => {
  assert.doesNotThrow(() => assertSealFits('review', version('x'.repeat(EVENT_MAX_BYTES - 1000))));
  assert.throws(() => assertSealFits('review', version('x'.repeat(EVENT_MAX_BYTES))), /over the 524288-byte cap; make the content shorter/);
  // Escaping counts: a newline encodes as two bytes.
  assert.throws(() => assertSealFits('review', version('\n'.repeat(EVENT_MAX_BYTES / 2))), /cap/);
});

test('a gated call whose approval would pass the cap fails before anything is recorded', async () => {
  let ran = false;
  let recorded = false;
  const tool = gated({ name: 'post_comment', description: 'post', parameters: {} as never, replay: 'safe', execute: async () => { ran = true; return { output: [] }; } });
  const api = { taskId: 7, now: () => 0, conversationId: 'c', commit: async () => { recorded = true; throw new Error('not reached'); } };
  const result = await tool.execute({ body: 'y'.repeat(EVENT_MAX_BYTES) } as never, api as never, ctx);
  assert.equal(result.isError, true);
  assert.match(JSON.stringify(result.output), /post_comment was not run: its arguments are \d+ bytes as an approval, over the 524288-byte cap/);
  assert.deepEqual([ran, recorded], [false, false]);
});

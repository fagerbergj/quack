import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createIngestQueue, type QueuedItem } from '../src/queue.ts';

const item = (document: string): QueuedItem => ({ batch: 'b', document, name: document, version: '1', attempt: 0 });

test('an item whose answer cannot be recorded stays pending without being resubmitted, and the rest of the queue moves on', async () => {
  const pending = [item('a'), item('b'), item('c')];
  const runs: string[] = [];
  const logged: string[] = [];
  const queue = createIngestQueue({ pending: () => [...pending] }, async (i) => {
    runs.push(i.document);
    if (i.document === 'a') throw new Error('disk full');
    pending.splice(pending.findIndex((p) => p.document === i.document), 1);
  }, 2, (line) => logged.push(line));
  queue.pump();
  assert.deepEqual(runs, ['a', 'b'], 'two at once, oldest first');
  await queue.drained();
  assert.deepEqual(runs, ['a', 'b', 'c']);
  assert.deepEqual(pending.map((p) => p.document), ['a']);
  assert.deepEqual(logged, ['remarkable: ingest of a in batch b stays pending: disk full']);
});

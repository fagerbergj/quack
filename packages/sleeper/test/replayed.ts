import assert from 'node:assert/strict';
import type { RecordedSession } from '@quack/sdk';
import { manifest } from '../src/index.ts';

/** The evidence the manifest's adapters rebuild for a recorded card; exactly one of them judges each card kind. */
export async function replayed(record: RecordedSession, content: string) {
  const all = await Promise.all(manifest.evalAdapters!.filter((a) => a.kind === 'sleeper.cards').map((a) => a.judge!.evidence(record, content)));
  const hits = all.filter((e) => e !== undefined);
  assert.equal(hits.length, 1, `${hits.length} adapters judge this card`);
  return hits[0];
}

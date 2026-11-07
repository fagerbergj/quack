import assert from 'node:assert/strict';
import { asJudged, type SessionRecord } from '@quack/eval';
import type { Rubric } from '@quack/judge';
import type { SealedArtifact } from '@quack/sdk';
import { manifest } from '../src/index.ts';

const ids = (checks: Array<{ id: string }>) => checks.map((k) => k.id).sort();

/** The evidence the manifest's adapters rebuild for a recorded card, seen as its judge saw it; exactly one adapter judges it, with the checks the live judge ran. */
export async function replayed(record: SessionRecord, sealed: SealedArtifact) {
  const view = asJudged(record, sealed.kind, sealed.version);
  const adapters = manifest.evalAdapters!.filter((a) => a.kind === sealed.kind);
  const all = await Promise.all(adapters.map(async (a) => ({ a, ev: await a.judge!.evidence!(view, sealed.content) })));
  const hits = all.filter((h) => h.ev !== undefined);
  assert.equal(hits.length, 1, `${hits.length} adapters judge this card`);
  const rubric = hits[0].a.judge!.rubric as Rubric;
  assert.deepEqual(ids(rubric.criteria.flatMap((c) => c.checks)), ids((sealed.verdict as unknown as { checks: Array<{ id: string }> }).checks), 'the replay judges the checks the live judge ran');
  return hits[0].ev;
}

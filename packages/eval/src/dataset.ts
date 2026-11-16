import { createHash } from 'node:crypto';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { DatasetItem } from './langfuse.ts';
import type { SessionRecord } from './record.ts';

export type Label = 'good' | 'bad';

/** A recorded session as a dataset item. Docs outside `served` (no eval service replays them) stay unpinned, listed in `metadata.unpinned`; a replay reads them live. */
export function itemFrom(record: SessionRecord, opts: { agent: string; served: ReadonlySet<string>; label?: Label; note?: string }): DatasetItem {
  // Extension docs (the PR a review saw) are pinned so a replay reads the same world; harness and quack bookkeeping are not.
  const world = Object.entries(record.docs).filter(([kind]) => !/^(pi|quack)\./.test(kind));
  const pins = Object.fromEntries(world.filter(([kind]) => opts.served.has(kind)));
  const unpinned = world.map(([kind]) => kind).filter((kind) => !opts.served.has(kind));
  const final = Object.fromEntries(Object.entries(record.artifacts).map(([kind, versions]) => {
    const last = versions.at(-1)!;
    const v = (last.verdict ?? {}) as { score?: number; passed?: boolean; limitReached?: boolean };
    return [kind, { version: last.version, content: last.content, score: v.score ?? null, passed: v.passed ?? null, limitReached: v.limitReached ?? null }];
  }));
  const input = { inputs: record.inputs, pins } as JsonObject;
  const id = `${opts.agent}-${createHash('sha256').update(JSON.stringify(input)).digest('hex').slice(0, 16)}`;
  const recorded = { turns: record.turns, usage: record.usage, revisions: Math.max(0, ...Object.values(record.artifacts).map((v) => v.length)) };
  return { id, input, expectedOutput: { outcome: record.outcome, artifacts: final } as JsonObject, metadata: { agent: opts.agent, unpinned, label: opts.label ?? null, note: opts.note ?? null, recorded } };
}

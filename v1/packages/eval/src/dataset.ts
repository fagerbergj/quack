import { createHash } from 'node:crypto';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { DatasetItem } from './langfuse.ts';
import type { SessionRecord } from './record.ts';

export type Label = 'good' | 'bad';

/** A recorded session as a dataset item: its inputs and pinned outside-world docs in, its sealed artifacts and verdicts out (EV1). */
export function itemFrom(record: SessionRecord, opts: { agent: string; label?: Label; note?: string }): DatasetItem {
  // Extension docs (the PR a review saw) are pinned so a replay reads the same world; harness and quack bookkeeping are not.
  const pins = Object.fromEntries(Object.entries(record.docs).filter(([kind]) => !/^(pi|quack)\./.test(kind)));
  const final = Object.fromEntries(Object.entries(record.artifacts).map(([kind, versions]) => {
    const last = versions.at(-1)!;
    const v = (last.verdict ?? {}) as { score?: number; passed?: boolean; limitReached?: boolean };
    return [kind, { version: last.version, content: last.content, score: v.score ?? null, passed: v.passed ?? null, limitReached: v.limitReached ?? null }];
  }));
  const input = { inputs: record.inputs, pins } as JsonObject;
  const id = `${opts.agent}-${createHash('sha256').update(JSON.stringify(input)).digest('hex').slice(0, 16)}`;
  const recorded = { turns: record.turns, usage: record.usage, revisions: Math.max(0, ...Object.values(record.artifacts).map((v) => v.length)) };
  return { id, input, expectedOutput: { outcome: record.outcome, artifacts: final } as JsonObject, metadata: { agent: opts.agent, label: opts.label ?? null, note: opts.note ?? null, recorded } };
}

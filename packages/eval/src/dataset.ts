import { createHash } from 'node:crypto';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { Approval, Question } from '@quack/sdk';
import type { Policy } from './driver.ts';
import type { DatasetItem } from './langfuse.ts';
import type { SessionRecord } from './record.ts';

export type Label = 'good' | 'bad';

/** The recorded answers in the order they were asked, and the verdicts on decided approvals; a withdrawn or pending approval has none. */
function asksOf(docs: Record<string, JsonObject>): Policy {
  const questions = Object.values((docs['quack.questions'] as { items?: Record<string, Question> } | undefined)?.items ?? {});
  const approvals = Object.values((docs['quack.approvals'] as { items?: Record<string, Approval> } | undefined)?.items ?? {});
  return {
    answers: questions.filter((q) => q.answer).sort((a, b) => a.askedAt - b.askedAt).map((q) => q.answer!.choice),
    verdicts: approvals.filter((a) => a.verdict).sort((a, b) => a.requestedAt - b.requestedAt).map(({ verdict: v }) => (v!.decision === 'approve' ? 'approve' : { reject: v!.reason ?? '' })),
  };
}

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
  // The id follows inputs and pins only, so items recorded before asks were kept keep their ids.
  const id = `${opts.agent}-${createHash('sha256').update(JSON.stringify({ inputs: record.inputs, pins })).digest('hex').slice(0, 16)}`;
  const input = { inputs: record.inputs, pins, asks: asksOf(record.docs) } as JsonObject;
  const recorded = { turns: record.turns, usage: record.usage, revisions: Math.max(0, ...Object.values(record.artifacts).map((v) => v.length)) };
  return { id, input, expectedOutput: { outcome: record.outcome, artifacts: final } as JsonObject, metadata: { agent: opts.agent, unpinned, label: opts.label ?? null, note: opts.note ?? null, recorded } };
}

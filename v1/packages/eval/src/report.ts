import { randomUUID } from 'node:crypto';
import type { Langfuse } from './langfuse.ts';
import type { ReplayResult } from './replay.ts';

/** The numbers a replay is judged by (EVL-2): checker verdicts, cost in tokens, turns, revisions and wall time. */
export function scoresOf(r: ReplayResult): Record<string, number> {
  const finals = Object.values(r.record.artifacts).map((v) => v.at(-1)!);
  const verdicts = finals.map((f) => (f.verdict ?? {}) as { score?: number; passed?: boolean });
  return {
    checker_score: verdicts.length ? Math.min(...verdicts.map((v) => v.score ?? 0)) : 0,
    passed: verdicts.length && verdicts.every((v) => v.passed) ? 1 : 0,
    revisions: Math.max(0, ...Object.values(r.record.artifacts).map((v) => v.length)),
    turns: r.record.turns,
    seconds: r.seconds,
    // shortcut: tokens stand in for cost until cloud-equivalent rates are configured per model.
    input_tokens: r.record.usage.input + r.record.usage.cacheRead,
    output_tokens: r.record.usage.output,
    delivered: r.captured.length,
    answered: r.record.outcome.status === 'done' && !r.error ? 1 : 0,
  };
}

/** Records one replay as a trace with scores, linked into the run as a Langfuse experiment (EV1). */
export async function publish(lf: Langfuse, runName: string, r: ReplayResult) {
  const traceId = randomUUID();
  const scores = scoresOf(r);
  const output = { outcome: r.record.outcome, artifacts: Object.fromEntries(Object.entries(r.record.artifacts).map(([k, v]) => [k, v.at(-1)?.content ?? null])), captured: r.captured, error: r.error ?? null };
  await lf.ingest([
    { id: `${traceId}-trace`, type: 'trace-create', body: { id: traceId, name: `eval ${runName}`, input: r.item.input, output, metadata: { datasetItemId: r.item.id } } },
    ...Object.entries(scores).map(([name, value]) => ({ id: `${traceId}-${name}`, type: 'score-create', body: { id: `${traceId}-${name}`, traceId, name, value, dataType: 'NUMERIC' } })),
  ]);
  await lf.linkRun({ runName, datasetItemId: r.item.id, traceId });
  return scores;
}

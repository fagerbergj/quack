import { randomBytes } from 'node:crypto';
import type { Langfuse } from './langfuse.ts';
import type { ReplayResult } from './replay.ts';

/** Whether an agent's final artifacts block what they judged; undefined when they hold no verdict. */
export type Blocks = (finals: Record<string, string>) => boolean | undefined;

// EV2's known-bad cases count only if a replay blocks them, and known-good ones only if it does not; the checker can pass a review that misses the defect.
function labelScores(r: ReplayResult, blocks?: Blocks): Record<string, number> {
  const label = (r.item.metadata as { label?: string } | undefined)?.label;
  const blocked = blocks?.(Object.fromEntries(Object.entries(r.record.artifacts).map(([k, v]) => [k, v.at(-1)!.content])));
  if (blocked === undefined || !label) return {};
  return label === 'bad' ? { caught: blocked ? 1 : 0 } : { false_block: blocked ? 1 : 0 };
}

/** The numbers a replay is judged by (EVL-2): checker verdicts, cost in tokens, turns, revisions and wall time. */
export function scoresOf(r: ReplayResult, blocks?: Blocks): Record<string, number> {
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
    ...labelScores(r, blocks),
  };
}

/** Records one replay as a trace with scores, linked into the run as a Langfuse experiment (EV1). */
export async function publish(lf: Langfuse, runName: string, r: ReplayResult, blocks?: Blocks) {
  // OTLP trace ids are 16 random bytes in hex.
  const traceId = randomBytes(16).toString('hex');
  const scores = scoresOf(r, blocks);
  const output = { outcome: r.record.outcome, artifacts: Object.fromEntries(Object.entries(r.record.artifacts).map(([k, v]) => [k, v.at(-1)?.content ?? null])), captured: r.captured, error: r.error ?? null };
  const endMs = Date.now();
  await lf.trace({ traceId, name: `eval ${runName}`, input: r.item.input, output, startMs: endMs - r.seconds * 1000, endMs });
  await lf.ingest([
    ...Object.entries(scores).map(([name, value]) => ({ id: `${traceId}-${name}`, type: 'score-create', body: { id: `${traceId}-${name}`, traceId, name, value, dataType: 'NUMERIC' } })),
  ]);
  await lf.linkRun({ runName, datasetItemId: r.item.id, traceId });
  return scores;
}

const median = (v: number[]) => [...v].sort((a, b) => a - b)[Math.floor(v.length / 2)] ?? 0;

/** A run's headline: catches and false blocks by label, with median turns, time and tokens. */
export function summarize(rows: Record<string, number>[]): string {
  const sum = (k: string) => rows.filter((r) => k in r).reduce((n, r) => n + r[k], 0);
  const count = (k: string) => rows.filter((r) => k in r).length;
  return [
    `caught ${sum('caught')}/${count('caught')} bad`,
    `false blocks ${sum('false_block')}/${count('false_block')} good`,
    `passed ${sum('passed')}/${rows.length}`,
    ...['turns', 'seconds', 'input_tokens'].map((k) => `median ${k} ${median(rows.map((r) => r[k]))}`),
  ].join(', ');
}

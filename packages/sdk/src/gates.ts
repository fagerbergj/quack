import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { SealedArtifact } from './artifacts.ts';
import { onWaiting, waitingNow } from './signals.ts';

/** The points where a gate may wait the loop (SDK contract v13); only artifact.sealed is wired so far. */
export type GatePoint = 'tool.call.before' | 'generation.before' | 'turn.ended' | 'artifact.sealed';

/** Pass (its note is shown to whoever sealed), deny (the call fails and the run ends), or continue (the call fails with the feedback to act on). */
export type GateVerdict = { kind: 'pass'; note?: string } | { kind: 'deny'; reason: string } | { kind: 'continue'; feedback: string };

// shortcut: no seq or extension events yet; add both when observe and emit land.
export type GateEvent = { id: string; at: number; type: 'artifact.sealed'; data: SealedArtifact };

// shortcut: only the in-tool fields of the contract's ProcessorContext; the rest come with observe and emit.
export type ProcessorContext = { api?: ToolExecutionApi; ctx?: Context; now(): number };

type Gate = { run: (event: GateEvent, p: ProcessorContext) => Promise<GateVerdict | void>; timeoutMs: number };

// One process owns one session (see signals.ts), so registration is module state, in install order.
const gates: Gate[] = [];

const DEFAULT_TIMEOUT_MS = 300_000;

/** Registers a gate; gates at a point run in registration order, which core makes the install order. Returns an unsubscriber. */
export function gate(point: GatePoint, run: Gate['run'], opts: { timeoutMs?: number } = {}): () => void {
  if (point !== 'artifact.sealed') throw new Error(`gate point ${point} is not wired yet; only artifact.sealed is`);
  const g = { run, timeoutMs: opts.timeoutMs ?? DEFAULT_TIMEOUT_MS };
  gates.push(g);
  return () => void gates.splice(gates.indexOf(g) >>> 0, 1);
}

/** What a seal rejects with when a gate denies or continues; callers in code revise on continue and stop on deny. */
export class SealRefused extends Error {
  readonly verdict: Exclude<GateVerdict, { kind: 'pass' }>;
  constructor(verdict: Exclude<GateVerdict, { kind: 'pass' }>, message: string) {
    super(message);
    this.verdict = verdict;
  }
}

// Fail-closed bound on a gate; time spent waiting on a person (whileWaiting) does not count against it.
function bounded(work: Promise<GateVerdict | void>, ms: number): Promise<GateVerdict | void> {
  return new Promise((resolve, reject) => {
    let left = ms;
    let since = Date.now();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const arm = () => {
      since = Date.now();
      timer = setTimeout(() => resolve({ kind: 'deny', reason: 'gate timed out' }), left);
    };
    const pause = () => {
      if (!timer) return;
      clearTimeout(timer);
      timer = undefined;
      left -= Date.now() - since;
    };
    const off = onWaiting((n) => (n > 0 ? pause() : timer ?? arm()));
    if (!waitingNow()) arm();
    work.then(resolve, reject).finally(() => {
      pause();
      off();
    });
  });
}

// Each seal in one execution of a task gets its own ordinal, so a re-seal reruns the chain while a replay of the task finds its records.
const ordinals = new WeakMap<object, number>();

/** Runs the artifact.sealed chain: the pass notes in gate order, or SealRefused on the first deny or continue. Each verdict is recorded under the sealing task. */
export async function runSealGates(api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<string[]> {
  const n = ordinals.get(api) ?? 0;
  ordinals.set(api, n + 1);
  const event: GateEvent = { id: `artifact:${sealed.kind}:${sealed.version}`, at: api.now(), type: 'artifact.sealed', data: sealed };
  const notes: string[] = [];
  for (const [i, g] of [...gates].entries()) {
    const key = `gate:artifact.sealed:${sealed.kind}:v${sealed.version}:${n}:${i}`;
    const verdict = (await api.memo<GateVerdict>(key, ctx)) ?? (await api.memo<GateVerdict>(key, (await bounded(g.run(event, { api, ctx, now: () => api.now() }), g.timeoutMs)) ?? { kind: 'pass' }, ctx));
    if (verdict.kind === 'deny') throw new SealRefused(verdict, verdict.reason);
    if (verdict.kind === 'continue') throw new SealRefused(verdict, verdict.feedback);
    if (verdict.note) notes.push(verdict.note);
  }
  return notes;
}

import type { Context } from '@earendil-works/chord/context';
import type { JsonValue } from '@earendil-works/chord';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { SealedArtifact } from './artifacts.ts';
import type { Approval } from './gate.ts';
import { onWaiting, waitingNow } from './signals.ts';

/** The points where a gate may wait the loop; artifact.sealed and approval.approving are wired so far. */
export type GatePoint = 'tool.call.before' | 'generation.before' | 'turn.ended' | 'artifact.sealed' | 'approval.approving';

/** Pass (its note is shown to whoever sealed), deny (the call fails and the run ends), or continue (the call fails with the feedback to act on).
 * At approval.approving, continue rejects the approval by the checks, the feedback its reason; deny reopens it undecided; `detail` is kept. */
export type GateVerdict = { kind: 'pass'; note?: string; detail?: JsonValue } | { kind: 'deny'; reason: string } | { kind: 'continue'; feedback: string; detail?: JsonValue };

/** What each wired point's event carries: the sealed version, or the approval a person just approved, before their verdict is recorded. */
type EventData = { 'artifact.sealed': SealedArtifact; 'approval.approving': { id: string; approval: Approval } };
type Wired = keyof EventData;
const WIRED: GatePoint[] = ['artifact.sealed', 'approval.approving'];

// shortcut: no seq or extension events yet; add both when observe and emit land.
export type GateEvent<P extends Wired = Wired> = { id: string; at: number; type: P; data: EventData[P] };

// shortcut: only the in-tool fields of the contract's ProcessorContext; the rest come with observe and emit.
export type ProcessorContext = { api?: ToolExecutionApi; ctx?: Context; now(): number };

type Gate = { point: GatePoint; run: (event: never, p: ProcessorContext) => Promise<GateVerdict | void>; timeoutMs: number };

// One process owns one session (see signals.ts), so registration is module state, in install order.
const gates: Gate[] = [];

const DEFAULT_TIMEOUT_MS = 300_000;

/** Registers a gate; gates at a point run in registration order, which core makes the install order. Returns an unsubscriber. */
export function gate<P extends Wired>(point: P, run: (event: GateEvent<P>, p: ProcessorContext) => Promise<GateVerdict | void>, opts: { timeoutMs?: number } = {}): () => void {
  if (!WIRED.includes(point)) throw new Error(`gate point ${point} is not wired yet; only ${WIRED.join(' and ')} are`);
  const g: Gate = { point, run, timeoutMs: opts.timeoutMs ?? DEFAULT_TIMEOUT_MS };
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
  for (const [i, g] of gates.filter((x) => x.point === 'artifact.sealed').entries()) {
    const key = `gate:artifact.sealed:${sealed.kind}:v${sealed.version}:${n}:${i}`;
    const verdict = (await api.memo<GateVerdict>(key, ctx)) ?? (await api.memo<GateVerdict>(key, (await bounded(g.run(event as never, { api, ctx, now: () => api.now() }), g.timeoutMs)) ?? { kind: 'pass' }, ctx));
    if (verdict.kind === 'deny') throw new SealRefused(verdict, verdict.reason);
    if (verdict.kind === 'continue') throw new SealRefused(verdict, verdict.feedback);
    if (verdict.note) notes.push(verdict.note);
  }
  return notes;
}

/** What the approval.approving chain found on a person's approve: passed unless a gate denied or continued, `decided` unless one denied
 * (it could not run its checks), their texts, and the first gate's detail. */
export type ApprovalChecks = { at: number; passed: boolean; decided: boolean; feedback: string[]; detail: JsonValue | null };

/** Runs every approval.approving gate on approval `id`, in order; the approval records the result, so nothing here is memoized. */
export async function runApprovalGates(api: ToolExecutionApi, id: string, approval: Approval, ctx: Context): Promise<ApprovalChecks> {
  const event: GateEvent<'approval.approving'> = { id: `approval:${id}`, at: api.now(), type: 'approval.approving', data: { id, approval } };
  const feedback: string[] = [];
  let detail: JsonValue | null = null;
  let decided = true;
  for (const g of gates.filter((x) => x.point === 'approval.approving')) {
    const v = (await bounded(g.run(event as never, { api, ctx, now: () => api.now() }), g.timeoutMs)) ?? { kind: 'pass' };
    if (v.kind !== 'pass') feedback.push(v.kind === 'deny' ? v.reason : v.feedback);
    if (v.kind !== 'deny' && v.detail !== undefined) detail ??= v.detail;
    decided &&= v.kind !== 'deny';
  }
  return { at: api.now(), passed: !feedback.length, decided, feedback, detail };
}

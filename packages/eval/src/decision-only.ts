import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { PASS_ONLY, type CheckDecl } from '@quack/judge';

/** Why the decision model left an item to the judge model. */
export type Cause = 'declared' | 'unsure' | 'pass-only-no' | 'over-window';
export const CAUSES: Cause[] = ['declared', 'unsure', 'pass-only-no', 'over-window'];
/** An item under decision-only: the decision model's pass or standing no, an escalation (unanswered) by cause, or `unasked` once its check had decided. */
export type Outcome = 'pass' | 'no' | Cause | 'unasked';

/** The api a decision-only round judges on, and what it heard: decision-model probabilities and escalated items, by unit and check key. */
export type DecisionOnly = { api: ToolExecutionApi; probabilities: Map<string, number>; escalated: Set<string> };

const itemKey = (unit: string, ask: string) => `${unit}\t${ask}`;

/** The request's item ids under the memo name the judge stores its probabilities by, when the request packs units. */
function packedIds(request: unknown): [string, string[]] | undefined {
  const state = (request as { state?: { context?: unknown; items?: Array<{ id: string }> } }).state;
  if (state?.context === undefined || !Array.isArray(state.items)) return undefined;
  const ids = state.items.map((i) => i.id);
  return [ids.length === 1 ? ids[0] : `${ids[0]}+${ids.length}`, ids];
}

/** The api with no judge model: each escalation reads its decision memo and gets `answer`'s choice, or none; Clef is asked as usual.
 *  Its answers and the escalations are read off the memo names the sdk's decide and the judge's resolve store them under. */
export function decisionOnly(api: ToolExecutionApi, memo: string, answer?: (ask: string) => string): DecisionOnly {
  const probabilities = new Map<string, number>();
  const escalated = new Set<string>();
  const packed = new Map<string, string[]>();
  const models = new Proxy(api.models, { get: (t, k) => {
    if (k === 'getModel') return () => undefined;
    if (k === 'completeSimple') return () => Promise.reject(new Error('decision-only: the judge model was asked'));
    const v: unknown = Reflect.get(t, k);
    if (typeof v !== 'function') return v;
    if (k !== 'classify') return v.bind(t);
    return (model: unknown, request: unknown, ...rest: unknown[]) => {
      const ids = packedIds(request);
      if (ids) packed.set(...ids);
      return v.call(t, model, request, ...rest);
    };
  } });
  const heard = (name: string, value: Record<string, number> | null) => Object.entries(value ?? {}).forEach(([q, p]) => {
    const ids = packed.get(name);
    const hash = q.lastIndexOf('#');
    probabilities.set(ids && hash >= 0 ? itemKey(ids[Number(q.slice(hash + 1))], q.slice(0, hash)) : itemKey(name, q), p);
  });
  const decision = `decision:${memo}:`;
  const suffix = ':classifier';
  const memoOf = (key: string, ...rest: unknown[]) => {
    if (key.startsWith(decision)) {
      const name = key.slice(decision.length);
      const ask = name.slice(name.lastIndexOf(':') + 1);
      escalated.add(itemKey(name.slice(0, -ask.length - 1), ask));
      return Promise.resolve(answer && { choice: answer(ask), confidence: 1, reason: 'decision-only', resolver: 'decision-only' });
    }
    if (rest.length === 2 && key.startsWith(`${memo}:`) && key.endsWith(suffix)) heard(key.slice(memo.length + 1, -suffix.length), rest[0] as Record<string, number> | null);
    return (api.memo as (...a: unknown[]) => Promise<unknown>)(key, ...rest);
  };
  const stub = new Proxy(api, { get: (t, k) => {
    if (k === 'models') return models;
    if (k === 'memo') return memoOf;
    const v: unknown = Reflect.get(t, k);
    return typeof v === 'function' ? v.bind(t) : v;
  } });
  return { api: stub, probabilities, escalated };
}

/** The option a check's item takes when it passes (or fails), as the judge reads a choice or bool answer. */
export function choiceOf(d: CheckDecl, pass: boolean): string {
  const options = d.question.type === 'choice' ? Object.keys(d.question.options) : ['yes', 'no'];
  const passing = d.question.type === 'choice' ? d.aggregate.pass ?? options.slice(0, 1) : d.question.pass ?? ['yes'];
  return options.find((o) => passing.includes(o) === pass) ?? options[0];
}

function outcomeOf(s: DecisionOnly, key: string, d: CheckDecl | undefined, act: number): Outcome {
  const p = s.probabilities.get(key);
  const passOnly = d?.decisionModel === PASS_ONLY;
  if (s.escalated.has(key)) {
    if (d?.decisionModel === false) return 'declared';
    if (p === undefined) return 'over-window';
    return passOnly && 1 - p >= act ? 'pass-only-no' : 'unsure';
  }
  if (p === undefined) return 'unasked';
  return p >= act ? 'pass' : 1 - p >= act && !passOnly ? 'no' : 'unasked';
}

/** Each item of one check (by its criterion.check key) the round heard of, by unit id. */
export function outcomes(s: DecisionOnly, ask: string, d: CheckDecl | undefined, act: number): Record<string, Outcome> {
  const units = [...s.probabilities.keys(), ...s.escalated].filter((k) => k.endsWith(`\t${ask}`)).map((k) => k.slice(0, -ask.length - 1));
  return Object.fromEntries([...new Set(units)].sort().map((u) => [u, outcomeOf(s, itemKey(u, ask), d, act)]));
}

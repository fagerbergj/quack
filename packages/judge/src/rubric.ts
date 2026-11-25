import type { JsonObject } from '@earendil-works/pi-durable';

/** A focused piece of evidence a check is judged on; `whole` marks the entire artifact, so its notes carry no unit prefix.
 * `shared` is context common to many units (an intent, a guide), sent once per packed decision-model request. */
export type Unit = {
  id: string;
  state: JsonObject;
  whole?: true;
  /** For a declared scope: which context selectors found matches for the item, and how many each. */
  related?: Record<string, number>;
  shared?: JsonObject;
};

/** Evidence units by scope name; the judge builds them from the declaration for the rubric's code checks. */
export type Evidence = Record<string, Unit[]>;

/** The facts the declaration's first `facts` unit carries, typed by the rubric that declared them; the cast is the rubric's claim. */
export const factsOf = <T>(ev: Evidence): T => ev.facts[0].state.facts as unknown as T;

/** `marks` names what in the artifact the result concerns (e.g. citation ids), so a renderer can flag it from the verdict alone. */
export type CodeResult = { met: number; detail?: string; marks?: string[] };

// One atomic check: a gate holds in every unit it applies to; a positive weight earns its share of met units;
// a negative weight is a pitfall that subtracts its share where present.
export type Check = {
  id: string;
  /** Plain words for what went wrong when this fails, shown to the user in place of the id. */
  label: string;
  weight: number | 'gate';
  /** Extra points a positive check subtracts per share missed, outside `possible`, for a miss that is also a pitfall. */
  miss?: number;
  code?: (artifact: string, evidence: Evidence) => CodeResult;
  /** When it runs: at every seal (the default), or only when a person approves the sealed version, through the approval.approving gate. */
  when?: 'seal' | 'approval';
};

export type Criterion = { id: string; checks: Check[] };
export type Rubric = { name: string; version: number; threshold: number; criteria: Criterion[] };

/** Why an item left the decision model: `declared` never asked of it, `unsure`, `pass-only-no` a confident no a pass-only check does not take, `over-window` too large or unanswered, `refused` a durable error, `outage` a request never answered (the item stays undecided). */
export type Cause = 'declared' | 'unsure' | 'pass-only-no' | 'over-window' | 'refused' | 'outage';
export const CAUSES: Cause[] = ['declared', 'unsure', 'pass-only-no', 'over-window', 'refused', 'outage'];

/** One check's result: a decided fold (true or false), the share of items that held, or null when no item applies or the fold is undecided. */
export type CheckResult = {
  criterion: string;
  id: string;
  label: string;
  weight: number | 'gate';
  met: boolean | number | null;
  units: number;
  /** This round's settlements by resolver: `code`, or the modelId of the model that answered (the decision model, or the judge model an escalation reached); with `reused` they cover the settled items. */
  resolvers: Record<string, number>;
  /** Items escalated from the decision model this round, plus those an outage left undecided, by cause; absent when none, and on verdicts from before it was recorded. */
  escalated?: Partial<Record<Cause, number>>;
  /** Items whose answer was reused from an earlier round in this session, by item id to the version first judged on; absent when none. */
  reused?: Record<string, number>;
  notes: string[];
  /** The items the fold left undecided while the check decided; listed so the reader sees what was not asked. */
  undecidedItems?: string[];
  /** Which splitter produced the items. */
  splitter?: string;
  /** Units no resolver decided; the check is then null, not unmet. */
  undecided?: number;
  marks?: string[];
  /** A grouped fold's groups the outer fold ran over; absent for other folds, and when no item applies or the split failed. */
  groups?: number;
  /** Not run on this verdict, so its met is null and the score leaves it out: it runs when a person approves the version, or once a version passes its code gates. */
  deferred?: Deferred;
};

/** Why a check did not run on a verdict. */
export type Deferred = 'approval' | 'gate';

export type Score = { score: number; gatesFailed: string[]; passed: boolean };

// Earned points over possible points; checks that apply to no unit count toward neither.
export function score(rubric: Rubric, results: CheckResult[]): Score {
  let earned = 0;
  let possible = 0;
  let penalty = 0;
  const gatesFailed: string[] = [];
  const miss = new Map(rubric.criteria.flatMap((c) => c.checks.map((k) => [`${c.id}.${k.id}`, k.miss ?? 0] as const)));
  for (const r of results) {
    if (r.met === null) continue;
    const met = Number(r.met);
    if (r.weight === 'gate') {
      if (met < 1) gatesFailed.push(`${r.criterion}.${r.id}`);
    } else if (r.weight > 0) {
      earned += r.weight * met;
      possible += r.weight;
      penalty += (miss.get(`${r.criterion}.${r.id}`) ?? 0) * (1 - met);
    } else {
      penalty += -r.weight * met;
    }
  }
  const value = possible > 0 ? Math.max(0, earned - penalty) / possible : penalty > 0 ? 0 : 1;
  return { score: value, gatesFailed, passed: gatesFailed.length === 0 && value >= rubric.threshold };
}

const fellShort = (r: CheckResult) => r.met !== null && r.weight !== 0 && (r.weight === 'gate' || r.weight > 0 ? Number(r.met) < 1 : Number(r.met) > 0);

/** Feedback names every failed gate and every check that fell short, with what the resolvers said; a weight-0 check is observe-only and never appears. */
export function feedback(results: CheckResult[]): string[] {
  return results.filter(fellShort).map((r) => {
    const kind = r.weight === 'gate' ? 'gate' : r.weight > 0 ? 'missed' : 'pitfall';
    // A grouped share counts groups, not items, and only when there is more than one; other folds count items.
    const n = r.groups ?? r.units;
    const counted = typeof r.met === 'number' && (r.groups === undefined ? r.units > 1 && !r.marks : r.groups > 1);
    const share = counted ? ` (${Math.round((r.met as number) * n)} of ${n})` : '';
    return `${kind} ${r.criterion}.${r.id}${share}${r.notes.length ? `: ${r.notes.join('; ')}` : ''}`;
  });
}

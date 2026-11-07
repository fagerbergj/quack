import type { JsonObject } from '@earendil-works/pi-durable';

/** A focused piece of evidence a check is judged on; `whole` marks the entire artifact, so its notes carry no unit prefix.
 * `shared` is context common to many units (an intent, a guide), sent once per decision-model request that packs them. */
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

/** `marks` names what in the artifact the result concerns (e.g. citation ids), so a renderer can flag it from the verdict alone. */
export type CodeResult = { met: number; detail?: string; marks?: string[] };

// One atomic check: a gate holds in every unit it applies to; a positive weight earns its share of met units;
// a negative weight is a pitfall that subtracts its share where present.
export type Check = {
  id: string;
  /** Plain words for what went wrong when this fails, shown to the user in place of the id. */
  label: string;
  weight: number | 'gate';
  /** @deprecated A rubric judged by `evidence` names its scope here; a declared rubric names it in `declaration.checks`. */
  scope?: string;
  /** @deprecated As `scope`: the per-unit question of a rubric still judged by `evidence`. */
  question?: string;
  code?: (artifact: string, evidence: Evidence) => CodeResult;
};

export type Criterion = { id: string; checks: Check[] };
export type Rubric = { name: string; version: number; threshold: number; criteria: Criterion[] };

/** One check's result: a decided fold (true or false), the share of items that held, or null when no item applies or the fold is undecided. */
export type CheckResult = {
  criterion: string;
  id: string;
  label: string;
  weight: number | 'gate';
  met: boolean | number | null;
  units: number;
  resolvers: Record<string, number>;
  notes: string[];
  /** The items the fold left undecided while the check decided; listed so the reader sees what was not asked. */
  undecidedItems?: string[];
  /** Which splitter produced the items, including a fallback (`sentences -> llm`). */
  splitter?: string;
  /** Why the fallback ran (the `expect` that the input did not meet). */
  splitterReason?: string;
  /** Units no resolver decided; the check is then null, not unmet. */
  undecided?: number;
  marks?: string[];
};

export type Score = { score: number; gatesFailed: string[]; passed: boolean };

export function unitsFor(scope: string | undefined, evidence: Evidence): Unit[] {
  if (!scope) throw new Error('a check with a question must name its evidence scope');
  return evidence[scope] ?? [];
}

// Earned points over possible points; checks that apply to no unit count toward neither.
export function score(rubric: Rubric, results: CheckResult[]): Score {
  let earned = 0;
  let possible = 0;
  let penalty = 0;
  const gatesFailed: string[] = [];
  for (const r of results) {
    if (r.met === null) continue;
    const met = Number(r.met);
    if (r.weight === 'gate') {
      if (met < 1) gatesFailed.push(`${r.criterion}.${r.id}`);
    } else if (r.weight > 0) {
      earned += r.weight * met;
      possible += r.weight;
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
    // A decided fold names its failing items in the notes; only a share says how many held.
    const share = typeof r.met === 'number' && r.units > 1 ? ` (${Math.round(r.met * r.units)} of ${r.units})` : '';
    return `${kind} ${r.criterion}.${r.id}${share}${r.notes.length ? `: ${r.notes.join('; ')}` : ''}`;
  });
}

import type { JsonObject } from '@earendil-works/pi-durable';

/** What a model check is asked about: the whole artifact, every finding, or only blocking findings. */
export type Scope = 'review' | 'finding' | 'blocking';

/** A focused piece of evidence a check is judged on; small enough for a decision model where possible. */
export type Unit = { id: string; state: JsonObject };
export type Evidence = { review: Unit; findings: Array<Unit & { blocking: boolean }> };

export type CodeResult = { met: number; detail?: string };

// One atomic check (D54): a gate holds in every unit it applies to; a positive weight earns its share of met units;
// a negative weight is a pitfall that subtracts its share where present.
export type Check = {
  id: string;
  weight: number | 'gate';
  scope?: Scope;
  question?: string;
  code?: (artifact: string, evidence: Evidence) => CodeResult;
};

export type Criterion = { id: string; checks: Check[] };
export type Rubric = { name: string; version: number; threshold: number; criteria: Criterion[] };

/** One check's result: the fraction of applicable units where it held, or null when no unit applies. */
export type CheckResult = {
  criterion: string;
  id: string;
  weight: number | 'gate';
  met: number | null;
  units: number;
  resolvers: Record<string, number>;
  notes: string[];
};

export type Score = { score: number; gatesFailed: string[]; passed: boolean };

export function unitsFor(scope: Scope | undefined, evidence: Evidence): Unit[] {
  if (scope === 'finding') return evidence.findings;
  if (scope === 'blocking') return evidence.findings.filter((f) => f.blocking);
  return [evidence.review];
}

// Earned points over possible points; checks that apply to no unit count toward neither.
export function score(rubric: Rubric, results: CheckResult[]): Score {
  let earned = 0;
  let possible = 0;
  let penalty = 0;
  const gatesFailed: string[] = [];
  for (const r of results) {
    if (r.met === null) continue;
    if (r.weight === 'gate') {
      if (r.met < 1) gatesFailed.push(`${r.criterion}.${r.id}`);
    } else if (r.weight > 0) {
      earned += r.weight * r.met;
      possible += r.weight;
    } else {
      penalty += -r.weight * r.met;
    }
  }
  const value = possible > 0 ? Math.max(0, earned - penalty) / possible : penalty > 0 ? 0 : 1;
  return { score: value, gatesFailed, passed: gatesFailed.length === 0 && value >= rubric.threshold };
}

/** Feedback names every failed gate and every check that fell short, with what the resolvers said. */
export function feedback(results: CheckResult[]): string[] {
  return results
    .filter((r) => r.met !== null && (r.weight === 'gate' || (typeof r.weight === 'number' && r.weight > 0) ? r.met < 1 : r.met > 0))
    .map((r) => {
      const kind = r.weight === 'gate' ? 'gate' : r.weight > 0 ? 'missed' : 'pitfall';
      const share = r.units > 1 ? ` (${Math.round(r.met! * r.units)} of ${r.units})` : '';
      return `${kind} ${r.criterion}.${r.id}${share}${r.notes.length ? `: ${r.notes.join('; ')}` : ''}`;
    });
}

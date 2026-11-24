import type { Context } from '@earendil-works/chord/context';
import { LiveDoc, type SetTimer, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { Artifacts, attachVerdict, inSessionSlot, sealedVersions, type SealedArtifact } from '@quack/sdk';
import { buildEvidence, checkDeclaration, PASS_ONLY, type AggregateDecl, type BuiltEvidence, type CheckDecl, type Declaration, type Inputs } from './declaration.ts';
import { fold, fractional, groupResults, metOf, outerOf, type Tri } from './fold.ts';
import { limiter } from './limit.ts';
import { answerItems, pack, passing, type Answer, type Answered, type Ask, type DecisionModelRef, type Hold, type Item, type ModelRef, type Pending } from './resolve.ts';
import { sessionReuse, type Kept, type Reuse } from './reuse.ts';
import { feedback, score, type CheckResult, type Evidence, type Rubric, type Unit } from './rubric.ts';

export * from './rubric.ts';
export { buildEvidence, PASS_ONLY, type AggregateDecl, type Chunk, type CheckDecl, type Declaration, type Inputs, type Question, type Scope, type SelectorDecl, type SplitDecl, type Where } from './declaration.ts';
export { registerSelector, registerSplitter } from './library.ts';
export { decisionModelTokens, type DecisionModelRef, type ModelRef } from './resolve.ts';
export { limiter } from './limit.ts';
export { judged, type JudgeConfig, type Judged } from './extension.ts';
export { manifest } from './manifest.ts';

/** The options that pass a declared check's item, as the judge reads an answer to it. */
export const passingOptions = (d: CheckDecl): string[] => passing({ id: '', question: d.question, pass: d.aggregate.pass });

export type JudgeOptions = {
  /** The artifact kind judged, by name. */
  kind: string;
  rubric: Rubric;
  /** The named texts the declaration reads, gathered per sealed version. */
  inputs: (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context) => Promise<Inputs>;
  /** How this artifact is judged: the scopes, and a question and fold per model check, keyed by rubric check id. */
  declaration: Declaration;
  /** Revise-and-recheck rounds before a failing version may go out with its failures marked: a round judges at most maxRevisions + 1 versions. */
  maxRevisions: number;
  /** A decision model asked first; uncertain items, and a unit that alone is over its input cap, escalate to the judge model; while it is unreachable its items stay undecided. */
  decisionModel?: DecisionModelRef;
  /** Where uncertain items escalate; the agent's own model when unset. */
  judgeModel?: ModelRef;
  /** Item reuse: an item whose question and evidence match one this session already decided takes that answer instead of asking; off when absent. */
  reuse?: boolean;
};

export type Verdict = { /** The run that judged it, so a run's submits can be counted. */ run?: string; passed: boolean; limitReached: boolean; score: number; gatesFailed: string[]; rubric: string; checks: CheckResult[]; undecided: string[] };

export function codeResults(rubric: Rubric, artifact: string, ev: Evidence): CheckResult[] {
  return rubric.criteria.flatMap((c) =>
    c.checks.filter((k) => k.code).map((k) => {
      const r = k.code!(artifact, ev);
      return { criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: r.met, units: 1, resolvers: { code: 1 }, notes: r.detail ? [r.detail] : [], marks: r.marks };
    }),
  );
}

/** One model check being judged: its units, the question asked of each, and the fold.
 *  `groupOf`: a grouped check's group key by unit id. */
export type Plan = { key: string; result: CheckResult; ask: Ask; units: Unit[]; aggregate: AggregateDecl; pitfall: boolean; answers: Map<string, Answer>; left: number; groupOf?: Record<string, string> };

const modelChecks = (rubric: Rubric) => rubric.criteria.flatMap((c) => c.checks.filter((k) => !k.code).map((k) => k.id));

function planChecks(rubric: Rubric, decl: Declaration, built: BuiltEvidence): Plan[] {
  return rubric.criteria.flatMap((c) => c.checks.filter((k) => !k.code).map((k): Plan => {
    const key = `${c.id}.${k.id}`;
    const d = decl.checks[k.id];
    const units = built.evidence[d.scope] ?? [];
    const split = built.splits[d.scope];
    const result: CheckResult = { criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: units.length ? 0 : null, units: units.length, resolvers: {}, notes: [], ...(split ? { splitter: split.splitter } : {}) };
    const ask: Ask = { id: key, question: d.question, pass: d.aggregate.pass, ...(d.decisionModel !== undefined && { decisionModel: d.decisionModel }) };
    const groupOf = d.aggregate.name === 'grouped' ? { groupOf: built.groups?.[d.scope] ?? {} } : {};
    return { key, result, ask, units, aggregate: d.aggregate, pitfall: typeof k.weight === 'number' && k.weight < 0, answers: new Map(), left: units.length, ...groupOf };
  }));
}

// An item code found related material for holds without a model under a share with `by`; `by: 'related'` never asks the model at all.
function preset(p: Plan, u: Unit): Answer | undefined {
  const by = p.aggregate.name === 'share' ? p.aggregate.by : undefined;
  if (!by || by === 'answer') return undefined;
  const found = Object.values(u.related ?? {}).some((n) => n > 0);
  return found ? { met: true, resolver: 'code' } : by === 'related' ? { met: false, resolver: 'code' } : undefined;
}

const triOf = (p: Plan, u: Unit): Tri => p.answers.get(u.id)?.met ?? undefined;
const trisOf = (p: Plan) => p.units.map((u) => triOf(p, u));
const groupsOf = (p: Plan) => groupResults(p.aggregate, p.units.map((u) => p.groupOf![u.id]), trisOf(p));
// What the fold runs over: the items, or, for a grouped fold, the groups' inner results under its outer rule.
const view = (ag: AggregateDecl, p: Plan): [AggregateDecl, Tri[]] => (ag.name === 'grouped' ? [outerOf(ag), [...groupsOf(p).values()]] : [ag, trisOf(p)]);
// A share is the result itself, so it is decided only once every item (group) is; a boolean fold decides as soon as none can change it.
function decided(p: Plan): boolean {
  const [ag, tris] = view(p.aggregate, p);
  return fractional(ag) ? tris.every((t) => t !== undefined) : fold(ag, tris) !== undefined;
}
// A grouped check's item is not asked once its own group has decided, either.
const groupDecided = (p: Plan, u: Unit) => p.aggregate.name === 'grouped' && groupsOf(p).get(p.groupOf![u.id]) !== undefined;

// Every failing item gets a note, even from a resolver that gives no reason (Clef never does): the item named, and the fix in the source,
// not the wording, since rewording cannot change what the source says.
const flaggedBy = (u: Unit, by: string) => {
  const text = Object.values(u.state).find((v) => typeof v === 'string')?.replace(/\s+/g, ' ').trim();
  const item = text ? `: "${text.slice(0, 60).trim()}${text.length > 60 ? '…' : ''}"` : '';
  return `flagged by ${by} (no reason given)${item}; re-verify it against the cited source before rewording it`;
};
const whyOf = (p: Plan, u: Unit) => {
  const a = p.answers.get(u.id)!;
  const on = p.result.reused?.[u.id];
  return `${a.note || flaggedBy(u, a.resolver)}${on ? ` (judged on v${on})` : ''}`;
};
const noteOf = (p: Plan, u: Unit) => (u.whole ? whyOf(p, u) : `${u.id}: ${whyOf(p, u)}`);

// One note per failing group, naming its first failing item and why; the failing groups' keys are the marks.
function groupedFinal(p: Plan, r: CheckResult): CheckResult {
  const keys = p.units.map((u) => p.groupOf![u.id]);
  const tris = trisOf(p);
  const groups = groupsOf(p);
  const failing = [...groups].filter(([, t]) => t === false).map(([k]) => k);
  r.notes = failing.map((k) => {
    const u = p.units[keys.findIndex((g, i) => g === k && tris[i] === false)];
    return `${k}: ${u.id}: ${whyOf(p, u)}`;
  });
  if (groups.size) r.groups = groups.size;
  if (failing.length) r.marks = failing;
  const open = [...groups].filter(([, t]) => t === undefined).map(([k]) => k);
  const [outer, results] = view(p.aggregate!, p);
  const value = fold(outer, results);
  if (value === undefined || (fractional(outer) && open.length)) return Object.assign(r, { met: null, undecided: tris.filter((t, i) => t === undefined && open.includes(keys[i])).length });
  r.met = metOf(outer, value, results, p.pitfall);
  if (open.length) r.undecidedItems = open;
  return r;
}

/** The check's result from its answers: a decided fold, or met null with the undecided count while the fold could still go either way. */
function finalize(p: Plan): CheckResult {
  const r = p.result;
  if (!p.units.length) return r;
  if (p.aggregate.name === 'grouped') return groupedFinal(p, r);
  const tris = p.units.map((u) => triOf(p, u));
  r.notes = p.units.flatMap((u, i) => (tris[i] === false ? [noteOf(p, u)] : []));
  const open = p.units.filter((_, i) => tris[i] === undefined).map((u) => u.id);
  const value = fold(p.aggregate, tris);
  if (value === undefined || (fractional(p.aggregate) && open.length)) return Object.assign(r, { met: null, undecided: open.length });
  r.met = metOf(p.aggregate, value, tris, p.pitfall);
  if (open.length) r.undecidedItems = open;
  return r;
}

/** A model check's items while it runs: settled by resolver (with `reused`), with the judge model now, and not yet answered. */
export type Counts = { units: number; settled: Record<string, number>; escalating: number; waiting: number };

/** What the check has decided so far, published as the tool call's running details; `met` is absent until the check is judged. */
export type Progress = { rubric: string; checks: Array<{ criterion: string; id: string; label: string; weight: number | 'gate'; met?: boolean | number | null } & Partial<Counts>> };

/** Hands progress to wherever it is shown; `setTimer` arms its trailing publish. */
export type Report = { send: (p: Progress) => Promise<void>; setTimer: SetTimer };

export function progressOf(rubric: Rubric, done: CheckResult[], counts = new Map<string, Counts>()): Progress {
  const byKey = new Map(done.map((r) => [`${r.criterion}.${r.id}`, r]));
  return { rubric: rubric.name, checks: rubric.criteria.flatMap((c) => c.checks.map((k) => ({ criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: byKey.get(`${c.id}.${k.id}`)?.met, ...counts.get(`${c.id}.${k.id}`) }))) };
}

const countsOf = (p: Plan, escalating: number): Counts => {
  const reused = Object.keys(p.result.reused ?? {}).length;
  return { units: p.units.length, settled: { ...p.result.resolvers, ...(reused && { reused }) }, escalating, waiting: p.left - escalating };
};

/** Sends at most once per `ms`: at once when the last send is that old, else in one trailing send of the state by then. */
function throttle(send: () => Promise<void>, now: () => number, setTimer: SetTimer, ms = 1000) {
  let last = -Infinity;
  let armed = false;
  let cancel: (() => void) | undefined;
  let flight = Promise.resolve();
  const fire = () => {
    armed = false;
    last = now();
    // Progress is only shown, so a failed send must not fail the round.
    flight = flight.then(send).catch(() => undefined);
  };
  return {
    poke() {
      if (armed) return;
      if (now() - last >= ms) return fire();
      armed = true;
      cancel = setTimer(fire, last + ms - now(), { background: true });
      // A host may decline a background timer; the next poke tries again rather than freezing the counts.
      if (!cancel) armed = false;
    },
    /** Nothing is sent once this resolves: the tool call's details close with the round. */
    async stop() {
      armed = true;
      cancel?.();
      await flight;
    },
  };
}

const wallTimer: SetTimer = (fn, ms) => {
  const t = setTimeout(fn, ms);
  return () => clearTimeout(t);
};

/** The model checks whose every item is answered and that decided; an undecided one stays out so it reads as still checking. */
export const settled = (plans: Plan[]): CheckResult[] => plans.filter((p) => !p.left).map((p) => ({ ...finalize(p) })).filter((r) => !r.undecided);

// Units asked of no model (covered by code) are answered up front; the rest are grouped by unit, all their checks together.
function itemsOf(plans: Plan[]): Item[] {
  const byUnit = new Map<string, Item>();
  for (const p of plans) {
    for (const u of p.units) {
      const fixed = preset(p, u);
      if (fixed) {
        p.answers.set(u.id, fixed);
        p.result.resolvers.code = (p.result.resolvers.code ?? 0) + 1;
        p.left--;
        continue;
      }
      const entry = byUnit.get(u.id) ?? { unit: u, checks: [] };
      entry.checks.push(p.ask);
      byUnit.set(u.id, entry);
    }
  }
  return [...byUnit.values()];
}

function restore(p: Plan, u: Unit, kept: Kept) {
  p.answers.set(u.id, { met: kept.met, resolver: kept.resolver, note: kept.note });
  (p.result.reused ??= {})[u.id] = kept.version;
  p.left--;
}

// Answers this session already decided stand in for asking; the rest are asked as in a fresh round, whose need-based
// skip sees the reused answers, so a verdict reads the same whichever round gave its answers.
function reuseItems(items: Item[], byKey: Map<string, Plan>, reuse: Reuse): Item[] {
  const fresh = (it: Item, ask: Ask) => {
    const kept = reuse.answers[reuse.key(ask, it.unit)];
    if (kept) restore(byKey.get(ask.id)!, it.unit, kept);
    return !kept;
  };
  return items.map((it) => ({ unit: it.unit, checks: it.checks.filter((ask) => fresh(it, ask)) })).filter((it) => it.checks.length);
}

// Under pass-only the decision model can only pass an item, and one pass decides an `any` group, so such a group's
// items escalate only once the decision model has answered every one of them that it is asked.
const waits = (p: Plan) => p.aggregate.name === 'grouped' && p.aggregate.inner === 'any' && p.ask.decisionModel === PASS_ONLY;

function holds(byKey: Map<string, Plan>): Hold & { expect: (items: Item[]) => void } {
  const groups = new Map<string, { left: number } & PromiseWithResolvers<void>>();
  const of = (u: Unit, check: Ask) => {
    const p = byKey.get(check.id)!;
    return waits(p) ? `${check.id}\u0000${p.groupOf![u.id]}` : '';
  };
  const expect = (items: Item[]) => items.forEach((it) => it.checks.filter((c) => of(it.unit, c)).forEach((c) => {
    const key = of(it.unit, c);
    const g = groups.get(key) ?? { left: 0, ...Promise.withResolvers<void>() };
    g.left++;
    groups.set(key, g);
  }));
  const heard = (asked: Pending[]) => asked.forEach((a) => {
    const g = groups.get(of(a.unit, a.check));
    if (g && !--g.left) g.resolve();
  });
  return { expect, heard, ready: (u, c) => groups.get(of(u, c))?.promise };
}

// Escalations go one at a time, as the session's slot carries one call, so a queued one rechecks need() when its turn comes.
// Decision-model requests are not gated by the slot; they keep their own limit of one.
async function modelResults(api: ToolExecutionApi, opts: JudgeOptions, built: BuiltEvidence, memo: string, ctx: Context, code: CheckResult[], reuse?: Reuse, report?: Report): Promise<CheckResult[]> {
  const plans = planChecks(opts.rubric, opts.declaration, built);
  const byKey = new Map(plans.map((p) => [p.key, p]));
  const inFlight = new Map<string, number>();
  const counts = () => new Map(plans.map((p) => [p.key, countsOf(p, inFlight.get(p.key) ?? 0)]));
  const live = report && throttle(() => report.send(progressOf(opts.rubric, [...code, ...settled(plans)], counts())), () => api.now(), report.setTimer);
  const busy = (asked: Pending[], n: 1 | -1) => {
    for (const a of asked) inFlight.set(a.check.id, (inFlight.get(a.check.id) ?? 0) + n);
    live?.poke();
  };
  const settings = { ref: opts.decisionModel, judgeModel: opts.judgeModel, memo, clef: limiter(1), escalations: limiter(1), hold: holds(byKey), busy };
  const tally = async ({ unit, check, answer }: Answered) => {
    const p = byKey.get(check.id)!;
    p.left--;
    if (answer) p.answers.set(unit.id, answer);
    // Resolvers count settled items only; an item no model could answer is the check's undecided, and is not kept.
    if (answer && answer.met !== null) {
      p.result.resolvers[answer.resolver] = (p.result.resolvers[answer.resolver] ?? 0) + 1;
      await reuse?.keep(check, unit, answer);
    }
    live?.poke();
  };
  // An unsure item escalates only while its check's fold (and its group's) can still go either way.
  const need = (u: Unit, check: Ask) => !decided(byKey.get(check.id)!) && !groupDecided(byKey.get(check.id)!, u);
  const items = reuse ? reuseItems(itemsOf(plans), byKey, reuse) : itemsOf(plans);
  settings.hold.expect(items);
  live?.poke();
  // The round's model calls ride the session's own capacity slot.
  await inSessionSlot(() => Promise.all(pack(items, opts.decisionModel?.maxInputTokens).map((group) => answerItems(api, settings, group, ctx, tally, need)))).finally(() => live?.stop());
  return plans.map(finalize);
}

async function evidenceFor(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, ctx: Context): Promise<BuiltEvidence> {
  checkDeclaration(opts.declaration, modelChecks(opts.rubric));
  return buildEvidence(opts.declaration, await opts.inputs(api, sealed, ctx));
}

// The run's first input id (stable across tool rounds), or the task id for a workflow run that has no LiveDoc.run.
const runOf = async (api: ToolExecutionApi, ctx: Context) => String((await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0] ?? api.taskId ?? '');

/** Versions this run has judged since its last pass, passing or failing: the count the revision limit bounds. */
async function judgedInRun(api: ToolExecutionApi, kind: string, ctx: Context): Promise<number> {
  const run = await runOf(api, ctx);
  const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
  const mine = sealedVersions(doc, kind).filter((v) => (v.verdict as { run?: string } | null)?.run === run);
  // A person's change request after a pass, in the same run, gets a fresh budget.
  return mine.length - 1 - mine.findLastIndex((v) => (v.verdict as { passed?: boolean }).passed);
}

async function judgeVersion(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, ctx: Context, memo = `judge:${sealed.kind}:v${sealed.version}`, report?: Report): Promise<Verdict> {
  // Progress is only shown, so a failed write leaves the row at "Checking" rather than failing the submit.
  await report?.send(progressOf(opts.rubric, [])).catch(() => undefined);
  const built = await evidenceFor(api, opts, sealed, ctx);
  const reuse = opts.reuse ? await sessionReuse(api, opts, sealed.version, ctx) : undefined;
  const code = codeResults(opts.rubric, sealed.content, built.evidence);
  const model = await modelResults(api, opts, built, memo, ctx, code, reuse, report);
  const checks = [...code, ...model];
  const s = score(opts.rubric, checks);
  const rubric = `${opts.rubric.name}@${opts.rubric.version}`;
  // The same budget the submit tool enforces: failures in this run since its last pass, not the session's.
  const limit = (await judgedInRun(api, sealed.kind, ctx)) >= opts.maxRevisions;
  const undecided = checks.filter((k) => k.undecided).map((k) => `${k.criterion}.${k.id}`);
  return { run: await runOf(api, ctx), passed: s.passed, limitReached: !s.passed && limit, score: s.score, gatesFailed: s.gatesFailed, rubric, checks, undecided };
}

/** Judges a sealed version again under opts, without attaching the verdict; memo keys the per-call memoization only, and item reuse follows opts.reuse. */
export function rejudge(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, memo: string, ctx: Context): Promise<Verdict> {
  return judgeVersion(api, opts, sealed, ctx, memo);
}

/** What a caller is told when no model could answer some checks; the judge-health audit greps "gave no usable answer". */
export function undecidedMessage(ids: string[], toolName: string): string {
  return `The decision model or the judge model gave no usable answer for ${ids.join(', ')} (usually one is at capacity or unreachable), so nothing was scored. Call ${toolName} again.`;
}

export function report(sealed: SealedArtifact, v: Verdict, toolName: string, delivers = false) {
  const head = `${sealed.kind} v${sealed.version}: ${Math.round(v.score * 100)}% (${v.rubric})`;
  const lines = feedback(v.checks).map((l) => `- ${l}`).join('\n');
  // Notes under a pass read as a to-do list and drew revisions that replaced the passed version; the verdict keeps them for people.
  if (v.passed) return `${head}, passed.${delivers ? '' : ` Version ${sealed.version} is final for this run; finish without revising it.`}`;
  if (v.limitReached) return `${head}, failed:\n${lines}\nThe revision limit is reached.${delivers ? '' : ` Deliver version ${sealed.version} with these failures marked.`}`;
  return `${head}, failed:\n${lines}\nRevise the file to address each point, then call ${toolName} again.`;
}

/** One judging round: a deny past the revision limit, an undecided message (nothing attached), or the verdict, attached. */
export type Outcome = { deny: string } | { undecided: string } | { verdict: Verdict };

/** Judges a sealed version once: a version that carries a verdict keeps it, from any run. `setTimer` is the progress throttle's test seam. */
export async function check(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, toolName: string, ctx: Context, setTimer = wallTimer): Promise<Outcome> {
  if (sealed.verdict) return { verdict: sealed.verdict as unknown as Verdict };
  // Every judged version counts, passing or not; a run past the limit is looping, and another round cannot help.
  if ((await judgedInRun(api, sealed.kind, ctx)) > opts.maxRevisions) return { deny: `${sealed.kind} was already checked ${opts.maxRevisions + 1} times in this run, the most it gets. Stop: this run is over.` };
  // A workflow step seals from code, whose task shows no running details.
  const shown = typeof api.details === 'function' ? { send: (p: Progress) => api.details(p, ctx), setTimer } : undefined;
  const verdict = await judgeVersion(api, opts, sealed, ctx, `judge:${sealed.kind}:v${sealed.version}`, shown);
  // A verdict missing checks would pass or fail on a model outage; another submit asks again what this round left undecided.
  if (verdict.undecided.length) return { undecided: undecidedMessage(verdict.undecided, toolName) };
  await attachVerdict(api, sealed.kind, sealed.version, verdict as never, ctx);
  return { verdict };
}

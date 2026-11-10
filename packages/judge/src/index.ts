import type { Context } from '@earendil-works/chord/context';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool, GenerationTask, hook, LiveDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { Artifacts, attachVerdict, seal, type ArtifactKind, type SealedArtifact } from '@quack/sdk';
import { buildEvidence, checkDeclaration, type AggregateDecl, type BuiltEvidence, type Declaration, type Inputs } from './declaration.ts';
import { fold, fractional, metOf, type Tri } from './fold.ts';
import { limiter } from './limit.ts';
import { answerItems, modelSplit, pack, type Answer, type Answered, type Ask, type ClassifierRef, type Item, type ModelRef } from './resolve.ts';
import { feedback, score, unitsFor, type CheckResult, type Evidence, type Rubric, type Unit } from './rubric.ts';

export * from './rubric.ts';
export { buildEvidence, type AggregateDecl, type Chunk, type CheckDecl, type Declaration, type Inputs, type Question, type Scope, type SelectorDecl, type SplitDecl, type SplitExpect, type Where } from './declaration.ts';
export { registerSelector, registerSplitter } from './library.ts';
export { classifierTokens, type ClassifierRef, type ModelRef } from './resolve.ts';
export { limiter } from './limit.ts';

type Common = {
  kind: ArtifactKind;
  rubric: Rubric;
  /** Revise-and-recheck rounds before a failing version may go out with its failures marked. A round judges at most maxRevisions + 1 versions; one more submit is refused and ends the run. A pass ends a round. */
  maxRevisions: number;
  /** Text when this run has nothing to submit (a checkout found no work); the submit is then refused and ends the run. */
  refuse?: (api: ToolExecutionApi, ctx: Context) => Promise<string | undefined>;
  /** A decision model asked first; uncertain items, and a unit that alone is over its input cap, escalate to the judge model. */
  classifier?: ClassifierRef;
  /** Where uncertain items escalate; the agent's own model when unset. */
  judgeModel?: ModelRef;
  /** The most escalations to the judge model in flight at once; 4 by default. Decision-model requests stay one at a time. */
  escalations?: number;
  toolName?: string;
  /** Runs once a version passes or is released at the limit (e.g. delivers it); its text is appended to the submit result. */
  onCleared?: (sealed: SealedArtifact, verdict: Verdict, api: ToolExecutionApi, ctx: Context) => Promise<{ text: string; isError?: boolean } | undefined>;
};

type Declared = {
  /** The named texts the declaration reads, gathered per sealed version. */
  inputs: (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context) => Promise<Inputs>;
  /** How this artifact is judged: the scopes, and a question and fold per model check, keyed by rubric check id. */
  declaration: Declaration;
  evidence?: never;
};

// shortcut: an undeclared rubric builds its own evidence and asks Check.question per unit; drop once github, research and sleeper declare.
type Legacy = { evidence: (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context) => Promise<Evidence>; inputs?: never; declaration?: never };

export type JudgeOptions = Common & (Declared | Legacy);

export type Verdict = { /** The run that judged it, so a run's submits can be counted. */ run?: string; passed: boolean; limitReached: boolean; score: number; gatesFailed: string[]; rubric: string; checks: CheckResult[]; undecided: string[] };

export function codeResults(rubric: Rubric, artifact: string, ev: Evidence): CheckResult[] {
  return rubric.criteria.flatMap((c) =>
    c.checks.filter((k) => k.code).map((k) => {
      const r = k.code!(artifact, ev);
      return { criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: r.met, units: 1, resolvers: { code: 1 }, notes: r.detail ? [r.detail] : [], marks: r.marks };
    }),
  );
}

/** One model check being judged: its units, the question asked of each, and the fold; a legacy check has no aggregate and scores the share of units met. */
export type Plan = { key: string; result: CheckResult; ask: Ask; units: Unit[]; aggregate?: AggregateDecl; pitfall: boolean; answers: Map<string, Answer>; left: number; failed?: string };

const modelChecks = (rubric: Rubric) => rubric.criteria.flatMap((c) => c.checks.filter((k) => !k.code).map((k) => k.id));

function planChecks(rubric: Rubric, decl: Declaration | undefined, built: BuiltEvidence): Plan[] {
  return rubric.criteria.flatMap((c) => c.checks.filter((k) => !k.code && (decl || k.question)).map((k): Plan => {
    const key = `${c.id}.${k.id}`;
    const d = decl?.checks[k.id];
    const units = d ? built.evidence[d.scope] ?? [] : unitsFor(k.scope, built.evidence);
    const split = d && built.splits[d.scope];
    const result: CheckResult = { criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: units.length ? 0 : null, units: units.length, resolvers: {}, notes: [], ...(split ? { splitter: split.splitter } : {}), ...(split?.reason ? { splitterReason: split.reason } : {}) };
    const ask: Ask = d ? { id: key, question: d.question, pass: d.aggregate.pass } : { id: key, question: { type: 'bool', instructions: k.question! } };
    return { key, result, ask, units, aggregate: d?.aggregate, pitfall: typeof k.weight === 'number' && k.weight < 0, answers: new Map(), left: units.length, failed: split?.failed };
  }));
}

// coverageUnion counts an item covered when code found related material; `by: 'related'` never asks the model at all.
function preset(p: Plan, u: Unit): Answer | undefined {
  if (p.aggregate?.name !== 'coverageUnion' || p.aggregate.by === 'answer') return undefined;
  const found = Object.values(u.related ?? {}).some((n) => n > 0);
  return found ? { met: true, resolver: 'code' } : p.aggregate.by === 'related' ? { met: false, resolver: 'code' } : undefined;
}

const triOf = (p: Plan, u: Unit): Tri => p.answers.get(u.id)?.met ?? undefined;
// A share is the result itself, so it is decided only once every item is; a boolean fold decides as soon as no item can change it.
const decided = (p: Plan) => !!p.aggregate && (fractional(p.aggregate) ? p.units.every((u) => triOf(p, u) !== undefined) : fold(p.aggregate, p.units.map((u) => triOf(p, u))) !== undefined);

// Every failing item gets a note, even from a resolver that gives no reason (Clef never does), so the reader sees which failed.
const noteOf = (u: Unit, a: Answer) => {
  const why = a.note || `flagged by ${a.resolver} (no reason given)`;
  return u.whole ? why : `${u.id}: ${why}`;
};

// A legacy check notes a "no" on a gate or a positive check and a "yes" on a pitfall.
function legacyFinal(p: Plan, r: CheckResult) {
  const answers = p.units.map((u) => p.answers.get(u.id));
  r.notes = p.units.flatMap((u, i) => (answers[i] && answers[i]!.met === p.pitfall ? [noteOf(u, answers[i]!)] : []));
  const undecided = answers.filter((a) => !a || a.met === null).length;
  // Met units are counted, not summed as fractions: 23 sums of 1/23 fall short of 1 and would read as a miss.
  r.met = undecided ? null : answers.filter((a) => a?.met).length / p.units.length;
  if (undecided) r.undecided = undecided;
}

/** The check's result from its answers: a decided fold, or met null with the undecided count while the fold could still go either way. */
function finalize(p: Plan): CheckResult {
  const r = p.result;
  if (p.failed) return Object.assign(r, { met: null, undecided: 1, notes: [p.failed] });
  if (!p.units.length) return r;
  if (!p.aggregate) {
    legacyFinal(p, r);
    return r;
  }
  const tris = p.units.map((u) => triOf(p, u));
  r.notes = p.units.flatMap((u, i) => (tris[i] === false ? [noteOf(u, p.answers.get(u.id)!)] : []));
  const open = p.units.filter((_, i) => tris[i] === undefined).map((u) => u.id);
  const value = fold(p.aggregate, tris);
  if (value === undefined || (fractional(p.aggregate) && open.length)) return Object.assign(r, { met: null, undecided: open.length });
  r.met = metOf(p.aggregate, value, tris, p.pitfall);
  if (open.length) r.undecidedItems = open;
  return r;
}

/** What the check has decided so far, published as the tool call's running details; `met` is absent until the check is judged. */
export type Progress = { rubric: string; checks: Array<{ criterion: string; id: string; label: string; weight: number | 'gate'; met?: boolean | number | null }> };

/** Hands progress to wherever it is shown. */
export type Report = (p: Progress) => Promise<void>;

export function progressOf(rubric: Rubric, done: CheckResult[]): Progress {
  const byKey = new Map(done.map((r) => [`${r.criterion}.${r.id}`, r]));
  return { rubric: rubric.name, checks: rubric.criteria.flatMap((c) => c.checks.map((k) => ({ criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: byKey.get(`${c.id}.${k.id}`)?.met }))) };
}

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

// Every request starts at once: decision-model requests queue on their own limit (one at a time) and escalations on theirs.
async function modelResults(api: ToolExecutionApi, opts: JudgeOptions, built: BuiltEvidence, memo: string, ctx: Context, code: CheckResult[], report?: Report): Promise<CheckResult[]> {
  const plans = planChecks(opts.rubric, opts.declaration, built);
  const byKey = new Map(plans.map((p) => [p.key, p]));
  const items = itemsOf(plans);
  const publish = async () => report?.(progressOf(opts.rubric, [...code, ...settled(plans)]));
  await publish();
  const settings = { ref: opts.classifier, judgeModel: opts.judgeModel, memo, clef: limiter(1), escalations: limiter(opts.escalations ?? 4) };
  const tally = async ({ unit, check, answer }: Answered) => {
    const p = byKey.get(check.id)!;
    p.left--;
    if (answer) {
      p.answers.set(unit.id, answer);
      p.result.resolvers[answer.resolver] = (p.result.resolvers[answer.resolver] ?? 0) + 1;
    }
    if (!p.left) await publish();
  };
  // An unsure item escalates only while its check's fold can still go either way.
  const need = (_u: Unit, check: Ask) => !decided(byKey.get(check.id)!);
  await Promise.all(pack(items, opts.classifier?.maxInputTokens).map((group) => answerItems(api, settings, group, ctx, tally, need)));
  return plans.map(finalize);
}

// A declared rubric's evidence is built from its inputs; a legacy one hands its evidence over as built.
async function evidenceFor(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, memo: string, ctx: Context): Promise<BuiltEvidence> {
  if (!opts.declaration) return { evidence: await opts.evidence(api, sealed, ctx), splits: {} };
  checkDeclaration(opts.declaration, modelChecks(opts.rubric));
  return buildEvidence(opts.declaration, await opts.inputs(api, sealed, ctx), modelSplit(api, opts.judgeModel, memo, ctx));
}

// The run's first input id: stable through the run, where its task id changes between tool rounds.
const runOf = async (api: ToolExecutionApi, ctx: Context) => String((await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0] ?? '');

/** Versions this run has judged since its last pass, passing or failing: the count the revision limit bounds. */
async function judgedInRun(api: ToolExecutionApi, kind: string, ctx: Context): Promise<number> {
  const run = await runOf(api, ctx);
  const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
  const mine = (doc?.kinds[kind]?.versions ?? []).filter((v) => (v.verdict as { run?: string } | null)?.run === run);
  // A person's change request after a pass, in the same run, gets a fresh budget.
  return mine.length - 1 - mine.findLastIndex((v) => (v.verdict as { passed?: boolean }).passed);
}

const terminal = (text: string) => ({ output: [{ type: 'text' as const, text }], isError: true, control: { terminate: true } });

async function judgeVersion(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, ctx: Context, memo = `judge:${sealed.kind}:v${sealed.version}`, report?: Report): Promise<Verdict> {
  await report?.(progressOf(opts.rubric, []));
  const built = await evidenceFor(api, opts, sealed, memo, ctx);
  const code = codeResults(opts.rubric, sealed.content, built.evidence);
  const checks = [...code, ...(await modelResults(api, opts, built, memo, ctx, code, report))];
  const s = score(opts.rubric, checks);
  const rubric = `${opts.rubric.name}@${opts.rubric.version}`;
  // The same budget the submit tool enforces: failures in this run since its last pass, not the session's.
  const limit = (await judgedInRun(api, sealed.kind, ctx)) >= opts.maxRevisions;
  const undecided = checks.filter((k) => k.undecided).map((k) => `${k.criterion}.${k.id}`);
  return { run: await runOf(api, ctx), passed: s.passed, limitReached: !s.passed && limit, score: s.score, gatesFailed: s.gatesFailed, rubric, checks, undecided };
}

/** Judges a sealed version again under opts, without attaching the verdict; memo keeps one replay's answers apart from another's. */
export function rejudge(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, memo: string, ctx: Context): Promise<Verdict> {
  return judgeVersion(api, opts, sealed, ctx, memo);
}

/** What a caller is told when the judge model could not answer some checks. */
export function undecidedMessage(ids: string[], toolName: string): string {
  return `The judge model gave no usable answer for ${ids.join(', ')} (usually it is at capacity), so nothing was scored. Call ${toolName} again.`;
}

function report(sealed: SealedArtifact, v: Verdict, toolName: string, delivers = false) {
  const head = `${sealed.kind} v${sealed.version}: ${Math.round(v.score * 100)}% (${v.rubric})`;
  const lines = feedback(v.checks).map((l) => `- ${l}`).join('\n');
  if (v.passed) return `${head}, passed.${delivers ? '' : ` Deliver version ${sealed.version}.`}${lines ? `\nMinor:\n${lines}` : ''}`;
  if (v.limitReached) return `${head}, failed:\n${lines}\nThe revision limit is reached.${delivers ? '' : ` Deliver version ${sealed.version} with these failures marked.`}`;
  return `${head}, failed:\n${lines}\nRevise the file to address each point, then call ${toolName} again.`;
}

// The judge as an extension: a durable tool seals and checks the artifact; the yield hook only refuses to finish too early.
export function judge(opts: JudgeOptions) {
  const toolName = opts.toolName ?? 'submit_artifact';
  if (opts.declaration) checkDeclaration(opts.declaration, modelChecks(opts.rubric));
  // A cleared version reaches onCleared on every submit, so a delivery that failed is retried; the hook must be idempotent.
  async function finish(sealed: SealedArtifact, v: Verdict, api: ToolExecutionApi, ctx: Context) {
    const cleared = v.passed || v.limitReached ? await opts.onCleared?.(sealed, v, api, ctx) : undefined;
    const text = [report(sealed, v, toolName, !!opts.onCleared), cleared?.text].filter(Boolean).join('\n');
    return { output: [{ type: 'text' as const, text }], ...(cleared?.isError ? { isError: true } : {}) };
  }

  const submit = defineTool({
    name: toolName,
    description: `Seal ${opts.kind.path} and check it against its rubric. Call it when the ${opts.kind.kind} is ready.`,
    parameters: obj({}, { open: true }),
    replay: 'safe',
    async execute(_args, api, ctx) {
      const refused = await opts.refuse?.(api, ctx);
      if (refused) return terminal(refused);
      let sealed: SealedArtifact;
      try {
        sealed = await seal(api, opts.kind, ctx);
      } catch (err) {
        return { output: [{ type: 'text', text: (err as Error).message }], isError: true };
      }
      if (sealed.verdict) return finish(sealed, sealed.verdict as unknown as Verdict, api, ctx);
      // Every judged version counts, passing or not; a run past the limit is looping, and another round cannot help.
      if ((await judgedInRun(api, opts.kind.kind, ctx)) > opts.maxRevisions) return terminal(`${opts.kind.kind} was already checked ${opts.maxRevisions + 1} times in this run, the most it gets. Stop: this run is over.`);
      const verdict = await judgeVersion(api, opts, sealed, ctx, `judge:${sealed.kind}:v${sealed.version}`, (p) => api.details(p, ctx));
      // A verdict missing checks would pass or fail on a model outage; answers are memoized per call, so another submit asks again.
      if (verdict.undecided.length) return { output: [{ type: 'text', text: undecidedMessage(verdict.undecided, toolName) }], isError: true };
      await attachVerdict(api, opts.kind.kind, sealed.version, verdict as never, ctx);
      return finish(sealed, verdict, api, ctx);
    },
  });

  return defineExtension({
    name: `judge:${opts.kind.kind}`,
    tools: [submit],
    hooks: [
      hook(GenerationTask, {
        async onYield(_answer, api, ctx) {
          const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
          const last = doc?.kinds[opts.kind.kind]?.versions.at(-1);
          const v = last?.verdict as Verdict | null | undefined;
          if (!last) return { continue: `Write ${opts.kind.path} and call ${toolName} before you finish.` };
          if (!v) return { continue: `Call ${toolName} to check ${opts.kind.path} before you finish.` };
          if (v.passed || v.limitReached) return undefined;
          return { continue: `${opts.kind.kind} v${last.version} failed its checks. Revise ${opts.kind.path} using the feedback, then call ${toolName} again.` };
        },
      }),
    ],
  });
}

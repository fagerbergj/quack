import type { Context } from '@earendil-works/chord/context';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool, GenerationTask, hook, LiveDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { Artifacts, attachVerdict, seal, type ArtifactKind, type SealedArtifact } from '@quack/sdk';
import { answerUnit, type ClassifierRef, type ModelRef } from './resolve.ts';
import { feedback, score, unitsFor, type Check, type CheckResult, type Evidence, type Rubric, type Unit } from './rubric.ts';

export * from './rubric.ts';
export type { ClassifierRef, ModelRef } from './resolve.ts';

export type JudgeOptions = {
  kind: ArtifactKind;
  rubric: Rubric;
  /** Builds the focused evidence units for a sealed version (the domain's job, e.g. diff excerpts per finding). */
  evidence: (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context) => Promise<Evidence>;
  /** Revise-and-recheck rounds before a failing version may go out with its failures marked (J1). A round judges at most maxRevisions + 1 versions; one more submit is refused and ends the run. A pass ends a round. */
  maxRevisions: number;
  /** Text when this run has nothing to submit (a checkout found no work); the submit is then refused and ends the run. */
  refuse?: (api: ToolExecutionApi, ctx: Context) => Promise<string | undefined>;
  /** A decision model asked first; uncertain or oversized checks escalate to the agent's own model. */
  classifier?: ClassifierRef;
  /** Where uncertain checks escalate; the agent's own model when unset. */
  judgeModel?: ModelRef;
  toolName?: string;
  /** Runs once a version passes or is released at the limit (e.g. delivers it); its text is appended to the submit result. */
  onCleared?: (sealed: SealedArtifact, verdict: Verdict, api: ToolExecutionApi, ctx: Context) => Promise<{ text: string; isError?: boolean } | undefined>;
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

function planModelChecks(rubric: Rubric, ev: Evidence) {
  const byUnit = new Map<string, { unit: Unit; checks: Check[] }>();
  const results = new Map<string, CheckResult>();
  for (const c of rubric.criteria) {
    for (const k of c.checks.filter((x) => x.question)) {
      const key = `${c.id}.${k.id}`;
      const units = unitsFor(k.scope, ev);
      results.set(key, { criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: units.length ? 0 : null, units: units.length, resolvers: {}, notes: [], scope: k.scope });
      for (const u of units) {
        const entry = byUnit.get(u.id) ?? { unit: u, checks: [] };
        entry.checks.push({ ...k, id: key });
        byUnit.set(u.id, entry);
      }
    }
  }
  return { byUnit, results };
}

/** What the check has decided so far, published as the tool call's running details; `met` is absent until every unit of the check is judged. */
export type Progress = { rubric: string; checks: Array<{ criterion: string; id: string; label: string; weight: number | 'gate'; met?: number | null }> };

/** Hands progress to wherever it is shown. */
export type Report = (p: Progress) => Promise<void>;

export function progressOf(rubric: Rubric, done: CheckResult[]): Progress {
  const byKey = new Map(done.map((r) => [`${r.criterion}.${r.id}`, r]));
  return { rubric: rubric.name, checks: rubric.criteria.flatMap((c) => c.checks.map((k) => ({ criterion: c.id, id: k.id, label: k.label, weight: k.weight, met: byKey.get(`${c.id}.${k.id}`)?.met }))) };
}

/** The model checks whose every unit is judged and decided; a partial or undecided one stays out so it reads as still checking. */
export function settled(results: Map<string, CheckResult>, left: Map<string, number>): CheckResult[] {
  return [...results].filter(([k, r]) => !left.get(k) && !r.undecided).map(([, r]) => r);
}

async function modelResults(api: ToolExecutionApi, opts: JudgeOptions, ev: Evidence, memo: string, ctx: Context, code: CheckResult[], report?: Report): Promise<CheckResult[]> {
  const { byUnit, results } = planModelChecks(opts.rubric, ev);
  const left = new Map([...results].map(([k, r]) => [k, r.units]));
  // A model check is final once every unit it applies to is judged; one with no unit is final at once.
  const publish = async () => report?.(progressOf(opts.rubric, [...code, ...settled(results, left)]));
  await publish();
  for (const { unit, checks } of byUnit.values()) {
    const answers = await answerUnit(api, opts.classifier, opts.judgeModel, memo, unit, checks, ctx);
    for (const [key, a] of Object.entries(answers)) {
      const r = results.get(key)!;
      left.set(key, left.get(key)! - 1);
      r.resolvers[a.resolver] = (r.resolvers[a.resolver] ?? 0) + 1;
      // No resolver decided: the check is unscored (null), never unmet, and the verdict lists it.
      if (a.met === null) r.undecided = (r.undecided ?? 0) + 1;
      if (a.met) r.met! += 1 / r.units;
      // A note explains a defect: a "no" on a gate or a positive check, a "yes" on a pitfall.
      if (a.note && a.met === (typeof r.weight === 'number' && r.weight < 0)) r.notes.push(unit.whole ? a.note : `${unit.id}: ${a.note}`);
    }
    await publish();
  }
  for (const r of results.values()) if (r.undecided) r.met = null;
  return [...results.values()];
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
  const ev = await opts.evidence(api, sealed, ctx);
  const code = codeResults(opts.rubric, sealed.content, ev);
  const checks = [...code, ...(await modelResults(api, opts, ev, memo, ctx, code, report))];
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
  return `The judge model gave no usable answer for ${ids.join(', ')} (usually it is at capacity), so nothing was scored. Call ${toolName} again; checks already answered are kept.`;
}

function report(sealed: SealedArtifact, v: Verdict, toolName: string, delivers = false) {
  const head = `${sealed.kind} v${sealed.version}: ${Math.round(v.score * 100)}% (${v.rubric})`;
  const lines = feedback(v.checks).map((l) => `- ${l}`).join('\n');
  if (v.passed) return `${head}, passed.${delivers ? '' : ` Deliver version ${sealed.version}.`}${lines ? `\nMinor:\n${lines}` : ''}`;
  if (v.limitReached) return `${head}, failed:\n${lines}\nThe revision limit is reached.${delivers ? '' : ` Deliver version ${sealed.version} with these failures marked.`}`;
  return `${head}, failed:\n${lines}\nRevise the file to address each point, then call ${toolName} again.`;
}

// The judge as an extension (D3): a durable tool seals and checks the artifact; the yield hook only refuses to finish too early.
export function judge(opts: JudgeOptions) {
  const toolName = opts.toolName ?? 'submit_artifact';
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
      // A verdict missing checks would pass or fail on a model outage; the decided answers are memoized, so calling again asks only the rest.
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
          if (last.carriedFrom) return undefined;
          if (!v) return { continue: `Call ${toolName} to check ${opts.kind.path} before you finish.` };
          if (v.passed || v.limitReached) return undefined;
          return { continue: `${opts.kind.kind} v${last.version} failed its checks. Revise ${opts.kind.path} using the feedback, then call ${toolName} again.` };
        },
      }),
    ],
  });
}

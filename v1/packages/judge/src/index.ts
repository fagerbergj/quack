import type { Context } from '@earendil-works/chord/context';
import { Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, GenerationTask, hook, type ToolExecutionApi } from '@earendil-works/pi-durable';
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
  /** Revise-and-recheck rounds before a failing version may go out with its failures marked (J1). */
  maxRevisions: number;
  /** A decision model asked first; uncertain or oversized checks escalate to the agent's own model. */
  classifier?: ClassifierRef;
  /** Where uncertain checks escalate; the agent's own model when unset. */
  judgeModel?: ModelRef;
  toolName?: string;
};

export type Verdict = { passed: boolean; limitReached: boolean; score: number; gatesFailed: string[]; rubric: string; checks: CheckResult[] };

function codeResults(rubric: Rubric, artifact: string, ev: Evidence): CheckResult[] {
  return rubric.criteria.flatMap((c) =>
    c.checks.filter((k) => k.code).map((k) => {
      const r = k.code!(artifact, ev);
      return { criterion: c.id, id: k.id, weight: k.weight, met: r.met, units: 1, resolvers: { code: 1 }, notes: r.detail ? [r.detail] : [], marks: r.marks };
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
      results.set(key, { criterion: c.id, id: k.id, weight: k.weight, met: units.length ? 0 : null, units: units.length, resolvers: {}, notes: [] });
      for (const u of units) {
        const entry = byUnit.get(u.id) ?? { unit: u, checks: [] };
        entry.checks.push({ ...k, id: key });
        byUnit.set(u.id, entry);
      }
    }
  }
  return { byUnit, results };
}

async function modelResults(api: ToolExecutionApi, opts: JudgeOptions, ev: Evidence, memo: string, ctx: Context): Promise<CheckResult[]> {
  const { byUnit, results } = planModelChecks(opts.rubric, ev);
  for (const { unit, checks } of byUnit.values()) {
    const answers = await answerUnit(api, opts.classifier, opts.judgeModel, memo, unit, checks, ctx);
    for (const [key, a] of Object.entries(answers)) {
      const r = results.get(key)!;
      r.resolvers[a.resolver] = (r.resolvers[a.resolver] ?? 0) + 1;
      if (a.met) r.met! += 1 / r.units;
      if (a.met === false && a.note) r.notes.push(`${unit.id}: ${a.note}`);
    }
  }
  return [...results.values()];
}

async function judgeVersion(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, ctx: Context, memo = `judge:${sealed.kind}:v${sealed.version}`): Promise<Verdict> {
  const ev = await opts.evidence(api, sealed, ctx);
  const checks = [...codeResults(opts.rubric, sealed.content, ev), ...(await modelResults(api, opts, ev, memo, ctx))];
  const s = score(opts.rubric, checks);
  const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
  const failedBefore = (doc?.kinds[sealed.kind]?.versions ?? []).filter((v) => v.verdict && !(v.verdict as { passed?: boolean }).passed).length;
  const rubric = `${opts.rubric.name}@${opts.rubric.version}`;
  const limit = failedBefore >= opts.maxRevisions;
  return { passed: s.passed, limitReached: !s.passed && limit, score: s.score, gatesFailed: s.gatesFailed, rubric, checks };
}

/** Judges a sealed version again under opts, without attaching the verdict; memo keeps one replay's answers apart from another's. */
export function rejudge(api: ToolExecutionApi, opts: JudgeOptions, sealed: SealedArtifact, memo: string, ctx: Context): Promise<Verdict> {
  return judgeVersion(api, opts, sealed, ctx, memo);
}

function report(sealed: SealedArtifact, v: Verdict, toolName: string) {
  const head = `${sealed.kind} v${sealed.version}: ${Math.round(v.score * 100)}% (${v.rubric})`;
  const lines = feedback(v.checks).map((l) => `- ${l}`).join('\n');
  if (v.passed) return `${head}, passed. Deliver version ${sealed.version}.${lines ? `\nMinor:\n${lines}` : ''}`;
  if (v.limitReached) return `${head}, failed:\n${lines}\nThe revision limit is reached: deliver version ${sealed.version} with these failures marked.`;
  return `${head}, failed:\n${lines}\nRevise the file to address each point, then call ${toolName} again.`;
}

// The judge as an extension (D3): a durable tool seals and checks the artifact; the yield hook only refuses to finish too early.
export function judge(opts: JudgeOptions) {
  const toolName = opts.toolName ?? 'submit_artifact';
  const submit = defineTool({
    name: toolName,
    description: `Seal ${opts.kind.path} and check it against its rubric. Call it when the ${opts.kind.kind} is ready.`,
    parameters: Type.Object({}),
    replay: 'safe',
    async execute(_args, api, ctx) {
      let sealed: SealedArtifact;
      try {
        sealed = await seal(api, opts.kind, ctx);
      } catch (err) {
        return { output: [{ type: 'text', text: (err as Error).message }], isError: true };
      }
      if (sealed.verdict) {
        const v = sealed.verdict as unknown as Verdict;
        return { output: [{ type: 'text', text: report(sealed, v, toolName) }] };
      }
      const verdict = await judgeVersion(api, opts, sealed, ctx);
      await attachVerdict(api, opts.kind.kind, sealed.version, verdict as never, ctx);
      return { output: [{ type: 'text', text: report(sealed, verdict, toolName) }] };
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

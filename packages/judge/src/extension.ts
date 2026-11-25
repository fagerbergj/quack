import type { Context } from '@earendil-works/chord/context';
import { defineExtension, GenerationTask, hook, LiveDoc, ROOT_CONVERSATION_ID, type Extension, type ToolExecutionApi } from '@earendil-works/pi-durable';
import type { JsonValue } from '@earendil-works/chord';
import { artifactKind, Artifacts, gate, latestSealed, sealedVersion, unsubmitted, type Approval, type ArtifactKind, type GateVerdict, type SealedArtifact } from '@quack/sdk';
import { checkDeclaration, type Declaration, type Inputs } from './declaration.ts';
import { check, checkApproval, hasApprovalChecks, report, type JudgeOptions, type Verdict } from './index.ts';
import type { DecisionModelRef, ModelRef } from './resolve.ts';
import type { Rubric } from './rubric.ts';

/** An agent's `extensions.judge`: the judge is on for that agent, with these settings. */
export type JudgeConfig = { maxRevisions?: number; decisionModel?: DecisionModelRef; judgeModel?: ModelRef };

/** How one artifact kind is judged. */
export type Judged = {
  rubric: Rubric;
  declaration: Declaration;
  inputs: (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context) => Promise<Inputs>;
  /** Text when this run has nothing to submit; the seal is then refused and the run ends. */
  refuse?: (api: ToolExecutionApi, ctx: Context) => Promise<string | undefined>;
};

// One process owns one session, so registrations are module state; a workflow re-registers its kind before each judged seal.
const registry = new Map<string, Judged>();

/** Registers how a kind is judged, replacing any earlier entry for it; a declaration that does not fit its rubric throws here. */
export function judged(kind: string, how: Judged): void {
  checkDeclaration(how.declaration, how.rubric.criteria.flatMap((c) => c.checks.filter((k) => !k.code).map((k) => k.id)));
  registry.set(kind, how);
}

// A judge round waits on the decision and judge models, which can queue behind other sessions well past the sdk's 5-minute default.
const JUDGE_TIMEOUT_MS = 60 * 60_000;

const toolOf = (kind: string) => artifactKind(kind)?.tool;

const optionsFor = (kind: string, how: Judged, config: JudgeConfig): JudgeOptions => ({ kind, rubric: how.rubric, declaration: how.declaration, inputs: how.inputs, maxRevisions: config.maxRevisions ?? 2, decisionModel: config.decisionModel, judgeModel: config.judgeModel, reuse: true });

async function verdictFor(api: ToolExecutionApi, ctx: Context, how: Judged, config: JudgeConfig, sealed: SealedArtifact): Promise<GateVerdict> {
  const refused = await how.refuse?.(api, ctx);
  if (refused) return { kind: 'deny', reason: refused };
  const tool = toolOf(sealed.kind) ?? 'the judge';
  const out = await check(api, optionsFor(sealed.kind, how, config), sealed, tool, ctx);
  if ('deny' in out) return { kind: 'deny', reason: out.deny };
  if ('undecided' in out) return { kind: 'continue', feedback: out.undecided };
  const text = report(sealed, out.verdict, tool);
  return out.verdict.passed || out.verdict.limitReached ? { kind: 'pass', note: text } : { kind: 'continue', feedback: text };
}

// A version written against another basis (a review of an older head) is overtaken, so it cannot be this run's submission.
async function overtaken(api: ToolExecutionApi, declared: ArtifactKind, last: SealedArtifact, ctx: Context) {
  const now = last.basis && (await declared.basis?.(api, ctx));
  return now && now !== last.basis ? `${declared.kind} v${last.version} was written against ${last.basis}, not ${now}. Write ${declared.path} for ${now} and call ${declared.tool} before you finish.` : undefined;
}

const MAX_UNSUBMITTED_RETRIES = 2;

// A run may not finish before each sealed-by-tool kind's newest version passed or was released at the limit and holds the run's last change to its file,
// unless the kind's refuse says there is nothing to submit.
const yieldHook = () => {
  let run = '';
  let unsubmittedRetries = 0;
  return hook(GenerationTask, {
    async onYield(_answer, hookApi, ctx) {
      // Only the root conversation delivers artifacts; owned child conversations such as subagents yield freely.
      if (hookApi.conversationId !== ROOT_CONVERSATION_ID) return undefined;
      // refuse and basis read committed state only, which the hook's api also serves.
      const api = hookApi as unknown as ToolExecutionApi;
      const current = String((await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0] ?? '');
      if (current !== run) [run, unsubmittedRetries] = [current, 0];
      const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
      for (const [kind, how] of registry) {
        const declared = artifactKind(kind);
        if (!declared?.tool) continue;
        const { path, tool } = declared;
        if (await how.refuse?.(api, ctx)) continue;
        const last = latestSealed(doc, kind);
        const v = last?.verdict as Verdict | null | undefined;
        if (!last) return { continue: `Write ${path} and call ${tool} before you finish.` };
        const stale = await overtaken(api, declared, last, ctx);
        if (stale) return { continue: stale };
        if (!v) return { continue: `Call ${tool} to check ${path} before you finish.` };
        if (!v.passed && !v.limitReached) return { continue: `${kind} v${last.version} failed its checks. Revise ${path} using the feedback, then call ${tool} again.` };
        const pending = await unsubmitted(api, declared, ctx);
        if (pending) {
          if (unsubmittedRetries >= MAX_UNSUBMITTED_RETRIES) return undefined;
          unsubmittedRetries++;
          return { continue: pending };
        }
      }
      return undefined;
    },
  });
};

// A person's approve of a registered kind's version runs the checks its seals left for then; the approval keeps the verdict as the detail.
async function approvalVerdict(api: ToolExecutionApi, ctx: Context, config: JudgeConfig, approval: Approval): Promise<GateVerdict | undefined> {
  const s = approval.subject;
  const how = s && registry.get(s.kind);
  if (!s || !how || !hasApprovalChecks(how.rubric)) return undefined;
  const sealed = await sealedVersion(api, s.kind, s.version, ctx);
  if (!sealed) return { kind: 'deny', reason: `${s.kind} v${s.version} is not sealed, so its checks cannot run` };
  const v = await checkApproval(api, optionsFor(s.kind, how, config), sealed, ctx, approval.approving?.at ?? 0);
  const detail = v as unknown as JsonValue;
  // An undecided check (an outage, or an item no model answered) is neither a pass nor a finding: the next approve asks again.
  if (v.undecided.length) return { kind: 'deny', reason: `The decision model or the judge model gave no usable answer for ${v.undecided.join(', ')}, so these checks did not run.` };
  return v.passed ? { kind: 'pass', detail } : { kind: 'continue', feedback: report(sealed, v, toolOf(s.kind) ?? 'the judge'), detail };
}

let unregister: (() => void) | undefined;

/** The judge's session half: its gates on artifact.sealed and approval.approving for registered kinds, and the yield hook for those with a seal tool. */
export function judgeSession(config: JudgeConfig): Extension[] {
  unregister?.();
  const offs = [
    gate('artifact.sealed', async (event, p) => {
      const how = registry.get(event.data.kind);
      return how && verdictFor(p.api!, p.ctx!, how, config, event.data);
    }, { timeoutMs: JUDGE_TIMEOUT_MS }),
    gate('approval.approving', (event, p) => approvalVerdict(p.api!, p.ctx!, config, event.data.approval), { timeoutMs: JUDGE_TIMEOUT_MS }),
  ];
  unregister = () => offs.forEach((off) => off());
  return [defineExtension({ name: 'judge', hooks: [yieldHook()] })];
}

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const countOk = (v: unknown, min: number) => v === undefined || (Number.isInteger(v) && (v as number) >= min);
const ref = (v: unknown, cap: boolean) => v === undefined || (isObject(v) && typeof v.provider === 'string' && typeof v.modelId === 'string' && (!cap || (Number.isInteger(v.maxInputTokens) && (v.maxInputTokens as number) > 0)));

/** Problems with an agent's `extensions.judge`. */
export function configProblems(config: unknown): string[] {
  if (config === undefined) return [];
  if (!isObject(config)) return ['judge must be an object'];
  const known = ['maxRevisions', 'decisionModel', 'judgeModel'];
  return [
    ...Object.keys(config).filter((k) => !known.includes(k)).map((k) => `judge.${k} is not a judge setting; known: ${known.join(', ')}`),
    ...(countOk(config.maxRevisions, 0) ? [] : ['judge.maxRevisions must be a non-negative integer']),
    ...(ref(config.decisionModel, true) ? [] : ['judge.decisionModel must be { provider, modelId, maxInputTokens }']),
    ...(ref(config.judgeModel, false) ? [] : ['judge.judgeModel must be { provider, modelId }']),
  ];
}

import type { Context } from '@earendil-works/chord/context';
import { defineExtension, type Extension, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { artifactKind, gate, type GateVerdict, type SealedArtifact } from '@quack/sdk';
import type { Declaration, Inputs } from './declaration.ts';
import { check, report, yieldHook, type JudgeOptions, type ToolKind } from './index.ts';
import type { DecisionModelRef, ModelRef } from './resolve.ts';
import type { Rubric } from './rubric.ts';

/** An agent's `extensions.judge`: the judge is on for that agent, with these settings. */
export type JudgeConfig = { maxRevisions?: number; decisionModel?: DecisionModelRef; judgeModel?: ModelRef; escalations?: number };

/** How one artifact kind is judged. */
export type Judged = {
  rubric: Rubric;
  declaration: Declaration;
  inputs: (api: ToolExecutionApi, sealed: { kind: string; version: number; content: string }, ctx: Context) => Promise<Inputs>;
  /** Text when this run has nothing to submit; the seal is then refused and the run ends. */
  refuse?: (api: ToolExecutionApi, ctx: Context) => Promise<string | undefined>;
};

// One process owns one session, so registrations are module state; a workflow re-registers its kind before each judged seal.
const registry = new Map<string, Judged>();

/** Registers how a kind is judged, replacing any earlier entry for it. */
export function judged(kind: string, how: Judged): void {
  registry.set(kind, how);
}

// A judge round waits on the decision and judge models, which can queue behind other sessions well past the sdk's 5-minute default.
const JUDGE_TIMEOUT_MS = 60 * 60_000;

const toolOf = (kind: string) => artifactKind(kind)?.tool;

async function verdictFor(api: ToolExecutionApi, ctx: Context, how: Judged, config: JudgeConfig, sealed: SealedArtifact): Promise<GateVerdict> {
  const refused = await how.refuse?.(api, ctx);
  if (refused) return { kind: 'deny', reason: refused };
  const tool = toolOf(sealed.kind) ?? 'the judge';
  const opts: JudgeOptions = { kind: sealed.kind, rubric: how.rubric, declaration: how.declaration, inputs: how.inputs, maxRevisions: config.maxRevisions ?? 2, decisionModel: config.decisionModel, judgeModel: config.judgeModel, escalations: config.escalations };
  const out = await check(api, opts, sealed, tool, ctx);
  if ('deny' in out) return { kind: 'deny', reason: out.deny };
  if ('undecided' in out) return { kind: 'continue', feedback: out.undecided };
  const text = report(sealed, out.verdict, tool);
  return out.verdict.passed || out.verdict.limitReached ? { kind: 'pass', note: text } : { kind: 'continue', feedback: text };
}

let unregister: (() => void) | undefined;

/** The judge's session half: its gate on artifact.sealed for registered kinds, and the yield hook for those with a seal tool. */
export function judgeSession(config: JudgeConfig): Extension[] {
  unregister?.();
  unregister = gate('artifact.sealed', async (event, p) => {
    const how = registry.get(event.data.kind);
    return how && verdictFor(p.api!, p.ctx!, how, config, event.data);
  }, { timeoutMs: JUDGE_TIMEOUT_MS });
  const toolKinds = (): ToolKind[] => [...registry.keys()].flatMap((kind) => {
    const k = artifactKind(kind);
    return k?.tool ? [{ kind, path: k.path, tool: k.tool }] : [];
  });
  return [defineExtension({ name: 'judge', hooks: [yieldHook(toolKinds)] })];
}

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const count = (v: unknown, min: number) => v === undefined || (Number.isInteger(v) && (v as number) >= min);
const ref = (v: unknown, cap: boolean) => v === undefined || (isObject(v) && typeof v.provider === 'string' && typeof v.modelId === 'string' && (!cap || (Number.isInteger(v.maxInputTokens) && (v.maxInputTokens as number) > 0)));

/** Problems with an agent's `extensions.judge`. */
export function configProblems(config: unknown): string[] {
  if (config === undefined) return [];
  if (!isObject(config)) return ['judge must be an object'];
  const known = ['maxRevisions', 'decisionModel', 'judgeModel', 'escalations'];
  return [
    ...Object.keys(config).filter((k) => !known.includes(k)).map((k) => `judge.${k} is not a judge setting; known: ${known.join(', ')}`),
    ...(count(config.maxRevisions, 0) ? [] : ['judge.maxRevisions must be a non-negative integer']),
    ...(count(config.escalations, 1) ? [] : ['judge.escalations must be a positive integer']),
    ...(ref(config.decisionModel, true) ? [] : ['judge.decisionModel must be { provider, modelId, maxInputTokens }']),
    ...(ref(config.judgeModel, false) ? [] : ['judge.judgeModel must be { provider, modelId }']),
  ];
}

import { Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, GenerationTask, hook } from '@earendil-works/pi-durable';
import { agentModel, Artifacts, attachVerdict, decide, seal, type ArtifactKind, type Resolver, type SealedArtifact } from '@quack/sdk';

/** One rubric criterion, judged pass or fail as its own decision point (JDG-5). */
export type Criterion = { id: string; question: string };

export type JudgeOptions = {
  kind: ArtifactKind;
  criteria: Criterion[];
  /** Revise-and-recheck rounds before a failing version may go out with its failures marked (J1). */
  maxRevisions: number;
  resolvers?: Resolver[];
  toolName?: string;
};

type CriterionResult = { id: string; passed: boolean; reason: string; resolver: string };
type Verdict = { passed: boolean; limitReached: boolean; criteria: CriterionResult[] };

async function check(api: Parameters<ReturnType<typeof defineTool>['execute']>[1], sealed: SealedArtifact, opts: JudgeOptions, ctx: Parameters<ReturnType<typeof defineTool>['execute']>[2]) {
  const results: CriterionResult[] = [];
  for (const c of opts.criteria) {
    const point = {
      name: `${sealed.kind}:v${sealed.version}:${c.id}`,
      question: c.question,
      options: ['pass', 'fail'],
      evidence: `${sealed.kind} v${sealed.version}:\n\n${sealed.content}`,
    };
    const d = await decide(api, point, opts.resolvers ?? [agentModel], ctx);
    // A criterion no resolver could decide counts as failed, so it is revised rather than waved through.
    results.push({ id: c.id, passed: d?.choice === 'pass', reason: d?.reason ?? 'no resolver gave a confident answer', resolver: d?.resolver ?? 'none' });
  }
  return results;
}

function report(sealed: SealedArtifact, v: Verdict, toolName: string) {
  const failed = v.criteria.filter((c) => !c.passed).map((c) => `- ${c.id}: ${c.reason}`).join('\n');
  if (v.passed) return `${sealed.kind} v${sealed.version} passed every check. Deliver version ${sealed.version}.`;
  if (v.limitReached) return `${sealed.kind} v${sealed.version} failed:\n${failed}\nThe revision limit is reached: deliver version ${sealed.version} with these failures marked.`;
  return `${sealed.kind} v${sealed.version} failed:\n${failed}\nRevise the file to address each point, then call ${toolName} again.`;
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
      if (sealed.verdict) return { output: [{ type: 'text', text: report(sealed, sealed.verdict as unknown as Verdict, toolName) }] };
      const criteria = await check(api, sealed, opts, ctx);
      const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
      const failedBefore = (doc?.kinds[opts.kind.kind]?.versions ?? []).filter((v) => v.verdict && !(v.verdict as { passed?: boolean }).passed).length;
      const passed = criteria.every((c) => c.passed);
      const verdict: Verdict = { passed, limitReached: !passed && failedBefore >= opts.maxRevisions, criteria };
      await attachVerdict(api, opts.kind.kind, sealed.version, verdict, ctx);
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

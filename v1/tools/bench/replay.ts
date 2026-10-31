// Judges every sealed review in benchmark sessions again under the current rubric, on copies, posting nothing.
// Usage: node tools/bench/replay.ts <deployment.json> <judge provider/model> <session-dir>...
import { cpSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { reviewEvidence, reviewerRubric, reviewKind } from '@quack/github';
import { feedback, rejudge, type Verdict } from '@quack/judge';
import { Artifacts } from '@quack/sdk';
import { buildModels, openSession, parseDeployment } from '@quack/session';

const [deploymentPath, judgeRef, ...dirs] = process.argv.slice(2);
if (!deploymentPath || !judgeRef?.includes('/') || !dirs.length) throw new Error('usage: replay.ts <deployment.json> <provider/model> <session-dir>...');
const slash = judgeRef.indexOf('/');
const judgeModel = { provider: judgeRef.slice(0, slash), modelId: judgeRef.slice(slash + 1) };
const deployment = parseDeployment(JSON.parse(readFileSync(deploymentPath, 'utf8')));
const classifier = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
const opts = { kind: reviewKind, rubric: reviewerRubric, evidence: reviewEvidence(), maxRevisions: 2, classifier, judgeModel };
const run = Date.now();

for (const dir of dirs) {
  const copy = join(mkdtempSync(join(tmpdir(), 'quack-replay-')), 'session');
  cpSync(dir, copy, { recursive: true });
  const lines: string[] = [];
  const replay = defineTool({
    name: 'replay_judge',
    description: 'Judge every sealed review again.',
    parameters: Type.Object({}),
    async execute(_args, api, ctx) {
      const versions = (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds[reviewKind.kind]?.versions ?? [];
      for (const sealed of versions) {
        const before = sealed.verdict as unknown as Verdict | null;
        const after = await rejudge(api, opts, sealed, `replay:${run}:v${sealed.version}`, ctx);
        const verdict = /## Verdict\n(\w+)/.exec(sealed.content)?.[1];
        const labels = [...sealed.content.matchAll(/^- (\w+)(?: \((\w+)\))?:/gm)].map((m) => m[1]).join(', ');
        lines.push(`  v${sealed.version} ${verdict} [${labels}]: ${before?.rubric} ${before?.score?.toFixed(2)} ${before?.passed ? 'pass' : 'fail'} -> ${after.rubric} ${after.score.toFixed(2)} ${after.passed ? 'pass' : 'fail'}`);
        for (const f of feedback(after.checks)) lines.push(`      ${f.slice(0, 400)}`);
      }
      return { output: [{ type: 'text', text: 'done' }] };
    },
  });
  const faux = fauxProvider({ models: [{ id: 'driver' }] });
  faux.setResponses([fauxAssistantMessage(fauxToolCall('replay_judge', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('replayed')]);
  const models = buildModels(deployment);
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: copy, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'replay', tools: [replay] })] } });
  await session.submit('Replay the judge.', `replay-${run}`, true);
  await session.close();
  rmSync(join(copy, '..'), { recursive: true, force: true });
  console.log(`${dir}\n${lines.join('\n')}`);
}

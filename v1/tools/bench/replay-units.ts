// Judges recorded evidence units under the current judge, N times, and prints met, resolvers and wall time per check.
// Usage: node tools/bench/replay-units.ts <cases.json> <repeats> <label> <out.json>; QUACK_LLM_URL is the llm-swap /v1 base, and QUACK_LOCAL_API_KEY / QUACK_CLEF_API_KEY must be set.
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { reviewerRubric } from '@quack/github';
import { rejudge, type Check, type Rubric } from '@quack/judge';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { digestRubric } from '../../packages/sleeper/src/report/rubric.ts';
import { waiversRubric } from '../../packages/sleeper/src/waivers/rubric.ts';

type Case = { case: string; kind: string; rubric: string; checks: string[]; content?: string; units: Record<string, Array<{ id: string; state: never }>> };
const [casesPath, repeats, label, outPath] = process.argv.slice(2);
if (!casesPath || !repeats || !label || !outPath) throw new Error('usage: replay-units.ts <cases.json> <repeats> <label> <out.json>');
const rubrics: Record<string, Rubric> = { reviewer: reviewerRubric, digest: digestRubric, waivers: waiversRubric };
const cases: Case[] = JSON.parse(readFileSync(casesPath, 'utf8'));
const llm = process.env.QUACK_LLM_URL!;
const deployment = parseDeployment({
  providers: {
    local: { baseUrl: llm, models: { 'qwen3.8-27b': { contextWindow: 262144, maxTokens: 16384 } } },
    clef: { baseUrl: `${llm.replace(/\/v1$/, '')}/upstream/clef/v1`, api: 'systemone', models: { clef: { contextWindow: 8192 } } },
  },
});
const classifier = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
const judgeModel = { provider: 'local', modelId: 'qwen3.8-27b' };
const only = (r: Rubric, ids: string[]): Rubric => ({ ...r, criteria: r.criteria.map((c) => ({ ...c, checks: c.checks.filter((k: Check) => ids.includes(k.id)) })).filter((c) => c.checks.length) });

const rows: unknown[] = [];
const replay = defineTool({
  name: 'replay_units',
  description: 'Judge every recorded case again.',
  parameters: Type.Object({}),
  async execute(_args, api, ctx) {
    for (let rep = 1; rep <= Number(repeats); rep++) {
      for (const c of cases) {
        const opts = { kind: { kind: 'replay', path: 'x' }, rubric: only(rubrics[c.rubric], c.checks.map((k) => k.split('.').pop()!)), evidence: async () => c.units, maxRevisions: 0, classifier, judgeModel };
        const t = Date.now();
        const v = await rejudge(api, opts, { kind: 'replay', version: 1, content: c.content ?? '' } as never, `${label}:${rep}:${c.case}`, ctx);
        const ms = Date.now() - t;
        for (const k of v.checks) rows.push({ label, rep, case: c.case, kind: c.kind, check: k.id, met: k.met, resolvers: k.resolvers, ms, note: k.notes[0] });
        console.log(label, rep, c.case, v.checks.map((k) => `${k.id}=${k.met}`).join(' '), `${ms}ms`);
      }
    }
    return { output: [{ type: 'text', text: 'done' }] };
  },
});
const faux = fauxProvider({ models: [{ id: 'driver' }] });
faux.setResponses([fauxAssistantMessage(fauxToolCall('replay_units', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('replayed')]);
const models = buildModels(deployment);
models.setProvider(faux.provider);
const m = faux.getModel();
const dir = mkdtempSync(join(tmpdir(), 'quack-replay-units-'));
const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'replay', tools: [replay] })] } });
await session.submit('Replay the units.', `replay-units-${label}`, true);
await session.close();
rmSync(dir, { recursive: true, force: true });
writeFileSync(outPath, JSON.stringify(rows, null, 1));

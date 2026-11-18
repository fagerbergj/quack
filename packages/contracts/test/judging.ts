import type { TestContext } from 'node:test';
import { createModels, fauxProvider } from '@earendil-works/pi-ai';
import { replayJudge } from '@quack/eval';
import { buildEvidence, rejudge, type Verdict } from '@quack/judge';
import { contractDeclaration, manifest, recordedSpecInputs, type ContractSpec } from '../src/index.ts';
import { fakeClef } from './clef.ts';

/** The judge's inputs for a spec sealed as version 1 of a session that recorded nothing else. */
export const inputsOf = (spec: ContractSpec) => {
  const content = JSON.stringify(spec);
  const sealed = { kind: 'contract.spec', version: 1, hash: '', content, sealedAt: 10, verdict: null };
  return recordedSpecInputs({ docs: {}, artifacts: { 'contract.spec': [sealed] }, readFile: async () => undefined }, content);
};

export const builtOf = async (spec: ContractSpec) => buildEvidence(contractDeclaration, await inputsOf(spec));
export const evidenceOf = async (spec: ContractSpec) => (await builtOf(spec)).evidence;

/** Judges `checks` with Clef answering by `pick`; the judge model has no replies, so a question that reached it would stay undecided. */
export async function judged(t: TestContext, spec: ContractSpec, checks: string[], pick: (q: string) => string) {
  const models = createModels();
  models.setProvider(fauxProvider({ provider: 'judge', models: [{ id: 'j' }] }).provider);
  const clef = await fakeClef(t, pick);
  models.setProvider(clef.provider);
  let verdict: Verdict | undefined;
  const capture: typeof rejudge = async (...args) => (verdict = await rejudge(...args));
  await replayJudge({
    cases: [{ case: 'spec', kind: 'contract.spec', checks, content: JSON.stringify(spec), inputs: await inputsOf(spec) }],
    arms: [{ label: 'clef', adapters: manifest.evalAdapters!, rejudge: capture, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }],
    repeat: 1, concurrency: 1, models, judgeModel: { provider: 'judge', modelId: 'j' },
  });
  return { check: (id: string) => verdict!.checks.find((k) => k.id === id)!, asked: clef.requests.flatMap((r) => Object.values(r.questions)), requests: clef.requests };
}

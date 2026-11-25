import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { num, obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { manifest as judgeManifest } from '@quack/judge';
import { answerQuestion, Approvals, Artifacts, closeQuestions, Failures, pendingQuestions, Questions, resolveApproval, seal, withdrawApprovals, type ArtifactKind } from '@quack/sdk';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { defineWorkflow, outcomeOf, startWorkflow, WorkflowDoc, workflowExtension } from '@quack/workflows';
import { contractsSessionExtensions } from '../src/index.ts';
import { settingsSpec } from './fixtures.ts';
import schema from './fixtures/durable-docs-contract-v13.json' with { type: 'json' };

// Lives here because this package's tests already reach the session store, the judge and the workflow engine (a feature package may
// import sdk and foundation packages), and its own reconcile writes the empty-summary artifacts.
const CONTRACT = { key: 'contracts:6fc57e77-dd2f-4458-9802-ded98054f946', version: 13 };
const ajv = new Ajv2020({ strict: false, validateFormats: false, allErrors: true });
ajv.addSchema(schema, 'root');
const def = (name: string) => ajv.compile({ $ref: `root#/$defs/${name}` });
const DOCS = { artifacts: def('artifactsDoc'), approvals: def('approvalsDoc'), questions: def('questionsDoc'), failures: def('failuresDoc'), workflow: def('workflowDoc') };
const DEFS = { principal: def('principal'), stepRecord: def('stepRecord'), sealedArtifact: def('sealedArtifact') };

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-durable-docs-')); dirs.push(d); return d; };
const jf = { kind: 'person', name: 'jf' } as const;
const tailnet = { kind: 'tailnet' } as const;

type Msgs = { messages: Array<{ content: unknown }> };
type Step = (c: Msgs) => ReturnType<typeof fauxAssistantMessage>;
const call = (name: string, args: Record<string, unknown>): Step => () => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });

async function openWith(replies: unknown[], extensions: unknown[], more: { turnCeiling?: number } = {}) {
  const faux = fauxProvider({ models: [{ id: 'stub' }] });
  faux.setResponses(replies as FauxResponseStep[]);
  const models = buildModels(parseDeployment({ defaultModel: 'unused/m', providers: { unused: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } }));
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return openSession({ dir: await tmp(), models, ...more, agent: { model: { provider: m.provider, modelId: m.id }, extensions: extensions as never } });
}
type Open = Awaited<ReturnType<typeof openWith>>;

// The five documents as the store holds them, read back after the real writers ran.
async function read(s: Open) {
  const at = (d: unknown) => s.harness.snapshot(d as never, s.root.id, ctx) as Promise<any>;
  return { artifacts: await at(Artifacts), approvals: await at(Approvals), questions: await at(Questions), failures: await at(Failures), workflow: await at(WorkflowDoc) };
}
function accepts(name: keyof typeof DOCS, doc: unknown) {
  assert.equal(DOCS[name](doc), true, `${name}: ${JSON.stringify(DOCS[name].errors)}`);
  assert.equal(ajv.validate('root', doc), true, `${name} against the root anyOf`);
}

// Resolves with the first value `pick` finds in a document; touching it first lets it be watched before anything has written it.
async function first<T>(s: Open, d: unknown, pick: (v: any) => T | undefined): Promise<T> {
  await s.root.commit(async (tx) => { await tx.doc(d as never, s.root.id); }, ctx);
  const watch = (await s.harness.watchDoc(d as never, s.root.id, ctx))!;
  return new Promise<T>((resolve) => {
    const hit = () => pick(watch.value);
    if (hit() !== undefined) return resolve(hit()!);
    watch.start(async () => { if (hit() !== undefined) resolve(hit()!); });
  }).finally(() => watch.stop());
}
const nextQuestion = (s: Open, seen: string[] = []) => first(s, Questions, (v) => pendingQuestions(v).find((q) => !seen.includes(q.id)));
const nextApproval = (s: Open, seen: string[] = []) => first(s, Approvals, (v) => Object.entries<{ verdict: unknown }>(v?.items ?? {}).find(([k, a]) => !a.verdict && !seen.includes(k))?.[0]);
const text = (c: Msgs) => { const m = c.messages.at(-1)!.content; return typeof m === 'string' ? m : (m as Array<{ text?: string }>).map((p) => p.text ?? '').join(''); };
// The judge's questions ("Decision: ...") all get yes; other requests take the next scripted step.
const judged = (script: Step[]): Step[] => {
  const answer = (_q: string) => ({ choice: 'yes', confidence: 1, reason: 'fine' });
  return Array.from({ length: 80 }, () => (c: Msgs) => {
    const p = text(c);
    if (p.startsWith('Decision:')) return fauxAssistantMessage(JSON.stringify(answer(p)));
    return script.shift()!(c);
  });
};
const ask = (id: string, question: string) => call('ask_question', { id, question, matters: 'It decides the shape.', suggested: 'Yes', alternatives: [{ answer: 'No', tradeoff: 'None.' }] });
const spec = (title: string) => call('write', { path: 'artifacts/spec.json', content: JSON.stringify({ ...settingsSpec(), title }, null, 2) });

test(`the checked-in schema is ${CONTRACT.key} v${CONTRACT.version}`, () => {
  assert.equal(schema.title, 'Quack durable session documents');
  assert.equal(schema.anyOf.length, 5);
});

test('a contract session writes judged and final artifacts, an empty summary, approvals and a closed question that pass the schema', async () => {
  const s = await openWith(
    judged([ask('scope', 'Closed or open file?'), spec('Sleeper settings file'), call('submit_contract', {}), call('submit_contract', {}), () => fauxAssistantMessage('Done.'), ask('later', 'Never answered?')]),
    [CodingTools, ...(await judgeManifest.session!({ maxRevisions: 0 }, {}, [])), ...contractsSessionExtensions()],
  );
  // The spec cites this file, so the judge's link check finds it in the workspace.
  const cited = 'packages/sleeper/src/service.ts';
  await mkdir(dirname(join(s.workspace, cited)), { recursive: true });
  await cp(join(dirname(fileURLToPath(import.meta.url)), '../../..', cited), join(s.workspace, cited));
  const run = s.submit('Define the Sleeper settings file', 'r-1', true);
  const q = await nextQuestion(s);
  assert.equal(await answerQuestion(s.root, q.id, q.options[0], tailnet, ctx), 'ok');
  const one = await nextApproval(s);
  assert.equal(await resolveApproval(s.root, one, { decision: 'reject', by: tailnet }, ctx), true);
  assert.equal(await resolveApproval(s.root, await nextApproval(s, [one]), { decision: 'approve', reason: 'ship it', by: jf }, ctx), true);
  assert.equal((await run).status, 'done');

  const second = s.submit('One more question', 'r-2', true);
  const open = await nextQuestion(s, [q.id]);
  await s.abort('service');
  await closeQuestions(s.root, ctx);
  await second;
  const docs = await read(s);
  assert.equal(docs.questions.items[open.id].closed, true);
  assert.deepEqual(docs.questions.items[q.id].answer.by, tailnet);
  const versions = docs.artifacts.kinds['contract.spec'].versions;
  assert.ok(versions.some((v: any) => v.final) && versions.every((v: any) => v.verdict), 'judged, and final once it passed');
  assert.ok(Object.values<any>(docs.artifacts.kinds).some((k) => k.versions.some((v: any) => v.summary === '')), 'a rejection with no reason reconciles to an empty summary');
  assert.deepEqual(Object.values<any>(docs.approvals.items).map((a) => a.verdict.by.kind), ['tailnet', 'person']);
  for (const name of ['artifacts', 'approvals', 'questions'] as const) accepts(name, docs[name]);
  // A withdrawn row as the SDK writes it, beside rows that predate the field.
  const unanswered = { ...Object.values<any>(docs.approvals.items)[0], verdict: null, executing: false, done: false };
  const withdrawn = { items: { ...docs.approvals.items, w: unanswered } };
  assert.deepEqual(withdrawApprovals(withdrawn, (id) => id === 'w', { at: 9, reason: 'superseded', supersededBy: 'contract.spec:2' }), ['w']);
  accepts('approvals', withdrawn);
  await s.close();
});

const note: ArtifactKind = { kind: 'test.note', path: 'note.md' };
const flow = defineWorkflow({
  name: 'docs',
  steps: [
    { name: 'gather', kind: 'code', run: async (c) => { await c.report({ progress: { done: 1, total: 2 }, cost: 0.5 }); await c.api.env!.writeFile('note.md', 'ducks', ctx); await seal(c.api, note, ctx); return { n: 1 }; } },
    { name: 'pick', kind: 'decision', point: () => ({ question: 'Who starts?', options: ['Pollard', 'Stevenson'] }), resolvers: [{ id: 'unsure', resolve: async () => undefined }] },
  ],
});

test('a workflow run, finished or aborted while waiting on a person, writes workflow and failure documents that pass the schema', async () => {
  const done = await openWith([], [workflowExtension([flow])]);
  const finished = outcomeOf(done.harness, await startWorkflow(done.harness, done.root, flow, null, 'Who starts?'));
  const asked = await nextQuestion(done);
  assert.equal(await answerQuestion(done.root, asked.id, 'Pollard', tailnet, ctx), 'ok');
  assert.equal((await finished).status, 'completed');
  const ok = await read(done);
  assert.deepEqual(ok.workflow.steps.map((r: any) => r.status), ['done', 'done']);
  assert.deepEqual(ok.workflow.steps[0].progress, { done: 1, total: 2 });
  for (const name of ['artifacts', 'questions', 'failures', 'workflow'] as const) accepts(name, ok[name]);
  await done.close();

  const stopped = await openWith([], [workflowExtension([flow])]);
  const id = await startWorkflow(stopped.harness, stopped.root, flow, null);
  const ended = outcomeOf(stopped.harness, id).catch(() => undefined);
  await nextQuestion(stopped);
  await stopped.harness.abortTask(Number(id) as never, ctx);
  await ended;
  const aborted = await read(stopped);
  assert.deepEqual(aborted.workflow.steps.map((r: any) => r.status), ['done', 'aborted']);
  accepts('workflow', aborted.workflow);
  accepts('failures', aborted.failures);
  await stopped.close();
});

test('the failure rows the runtime writes, runaway and model, pass the schema', async () => {
  const probe = defineTool({ name: 'probe', description: 'Looks something up', parameters: obj({ n: num() }, { open: true }), execute: async (a) => ({ output: [{ type: 'text', text: `result ${a.n}` }] }) });
  const loop = await openWith(Array.from({ length: 20 }, (_, n) => call('probe', { n })), [defineExtension({ name: 'probe', tools: [probe] })], { turnCeiling: 3 });
  await loop.submit('go', 'loop', true);
  const runaway = (await read(loop)).failures;
  assert.match(runaway.items.runaway.error, /more than 3 model turns/);
  accepts('failures', runaway);
  await loop.close();

  const broken = await openWith([fauxAssistantMessage('', { stopReason: 'error', errorMessage: '400 invalid request' })], []);
  await broken.submit('hi', 'model', true);
  const model = await first(broken, Failures, (v) => (v?.items?.model?.error ? v : undefined));
  assert.match(model.items.model.error, /400/);
  accepts('failures', model);
  await broken.close();
});

const step = { name: 'x', kind: 'code', status: 'done', startedAt: 1, durationMs: 1, cost: 0, error: null };
const sealedV = { kind: 'k', version: 1, hash: 'a'.repeat(64), content: 'x', sealedAt: 1, verdict: null };
const approvals = (by: unknown) => ({ items: { 1: { tool: 't', args: {}, requestedAt: 1, verdict: { decision: 'approve', reason: null, by, at: 2 }, executing: false, done: false } } });
const artifacts = (over: object) => ({ kinds: { k: { versions: [{ ...sealedV, ...over }] } } });

test('the schema still rejects what no writer produces, so a loosening is caught', () => {
  const rejects = (name: keyof typeof DOCS, doc: unknown, label: string) => assert.equal(DOCS[name](doc), false, label);
  rejects('approvals', approvals({ kind: 'person' }), 'a person without a name');
  rejects('approvals', approvals({ kind: 'person', name: '' }), 'a person with an empty name');
  rejects('approvals', approvals({ kind: 'tailnet', name: 'x' }), 'a tailnet principal with a name');
  rejects('approvals', approvals({ kind: 'person', id: 'x' }), 'a person with id instead of name');
  rejects('workflow', { name: 'w', task: '1', request: null, steps: [{ ...step, kind: 'gated' }] }, "a step kind 'gated'");
  rejects('workflow', { name: 'w', task: '1', request: null, steps: [{ ...step, status: 'cancelled' }] }, 'an unknown step status');
  rejects('artifacts', artifacts({ final: null }), 'a null final');
  rejects('artifacts', artifacts({ sealedAt: '2026-01-01T00:00:00Z' }), 'an ISO-string sealedAt');
  rejects('artifacts', artifacts({ hash: 'qa0' }), 'a hash that is not 64 hex');
  rejects('failures', { items: { a: { error: null, run: 1, at: '2026-01-01T00:00:00Z' } } }, 'an ISO-string at');
  for (const by of [jf, tailnet, 'jf']) assert.equal(DEFS.principal(by), true, JSON.stringify(by));
  assert.equal(DEFS.stepRecord({ ...step, status: 'aborted', progress: { done: 1, total: 2 } }), true);
  assert.equal(DEFS.sealedArtifact({ ...sealedV, final: { input: '5', refused: 0 }, summary: '' }), true);
});

import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, Type } from '@earendil-works/pi-ai';
import { defineTool } from '@earendil-works/pi-durable';
import { Approvals, Artifacts, pendingApprovals, resolveApproval, seal, type ArtifactKind } from '@quack/sdk';
import { openSession } from '@quack/session';
import { answerDecision, defineWorkflow, outcomeOf, runWorkflow, startWorkflow, watchUntil, WorkflowDoc, workflowExtension, type Workflow } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-wf-')); dirs.push(d); return d; };

function open(dir: string, workflow: Workflow, ...replies: string[]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(replies.map((r) => fauxAssistantMessage(r)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [workflowExtension([workflow])] } });
}

type Open = Awaited<ReturnType<typeof open>>;
const doc = async (s: Open) => (await s.harness.snapshot(WorkflowDoc, s.root.id, ctx))!;
const waitFor = async (s: Open, pick: Parameters<typeof watchUntil>[1]) => watchUntil((await s.harness.watchDoc(WorkflowDoc, s.root.id, ctx))!, pick);

const note: ArtifactKind = { kind: 'test.note', path: 'note.md' };

test('a code, model and code workflow completes, records its steps and seals its artifact', async () => {
  const workflow = defineWorkflow({
    name: 'three',
    steps: [
      { name: 'gather', kind: 'code', run: async (c) => { await c.api.env!.writeFile('note.md', 'ducks', ctx); await seal(c.api, note, ctx); return { items: [1, 2, 3] }; } },
      { name: 'rank', kind: 'model', schema: Type.Object({ top: Type.Number() }), prompt: (c) => `Pick the best of ${JSON.stringify(c.prior.gather)}` },
      { name: 'report', kind: 'code', run: (c) => `top is ${(c.prior.rank as { top: number }).top}` },
    ],
  });
  const s = await open(await tmp(), workflow, 'Sure: {"top": 3}');
  const outcome = await runWorkflow(s.harness, s.root, workflow, null);

  assert.equal(outcome.status, 'completed');
  assert.deepEqual(outcome.status === 'completed' && outcome.result, { gather: { items: [1, 2, 3] }, rank: { top: 3 }, report: 'top is 3' });
  const steps = (await doc(s)).steps;
  assert.deepEqual(steps.map((r) => [r.name, r.kind, r.status]), [['gather', 'code', 'done'], ['rank', 'model', 'done'], ['report', 'code', 'done']]);
  assert.ok(steps.every((r) => r.startedAt !== null && r.durationMs !== null && r.durationMs >= 0));
  const sealed = await s.harness.snapshot(Artifacts, s.root.id, ctx);
  assert.equal(sealed!.kinds['test.note'].versions[0].content, 'ducks');
  await s.close();
});

test('a crash mid-step resumes at that step without rerunning earlier ones', async () => {
  const runs = { first: 0, second: 0 };
  let entered: () => void;
  const inStep = new Promise<void>((r) => (entered = r));
  const build = (hang: boolean) => defineWorkflow({
    name: 'resume',
    steps: [
      { name: 'first', kind: 'code', run: () => ++runs.first },
      {
        name: 'second',
        kind: 'code',
        run: async (c) => {
          runs.second++;
          if (!hang) return 'finished';
          entered();
          await new Promise((_, reject) => c.signal.addEventListener('abort', () => reject(c.signal.reason)));
          return 'unreachable';
        },
      },
    ],
  });
  const dir = await tmp();
  const a = await open(dir, build(true));
  const running = runWorkflow(a.harness, a.root, build(true), null).catch(() => undefined);
  await inStep;
  await a.close();
  await running;

  const b = await open(dir, build(false));
  const outcome = await runWorkflow(b.harness, b.root, build(false), null);
  assert.deepEqual(outcome.status === 'completed' && outcome.result, { first: 1, second: 'finished' });
  assert.deepEqual(runs, { first: 1, second: 2 });
  assert.deepEqual((await doc(b)).steps.map((r) => r.status), ['done', 'done']);
  await b.close();
});

test('a model reply that fails the schema fails the step and stops the workflow', async () => {
  let ranAfter = false;
  const workflow = defineWorkflow({
    name: 'strict',
    steps: [
      { name: 'rank', kind: 'model', schema: Type.Object({ top: Type.Number() }), prompt: () => 'rank' },
      { name: 'after', kind: 'code', run: () => { ranAfter = true; return null; } },
    ],
  });
  const s = await open(await tmp(), workflow, '{"top": "three"}');
  const outcome = await runWorkflow(s.harness, s.root, workflow, null);

  assert.equal(outcome.status, 'failed');
  const [rank, after] = (await doc(s)).steps;
  assert.equal(rank.status, 'failed');
  assert.match(rank.error!, /step rank:.*top/s);
  assert.equal(after.status, 'pending');
  assert.equal(ranAfter, false);
  await s.close();
});

test('a decision step waits for a person across a restart and resumes on the answer', async () => {
  const build = () => defineWorkflow({
    name: 'decide',
    steps: [
      {
        name: 'pick',
        kind: 'decision',
        point: () => ({ question: 'Who starts?', options: ['Pollard', 'Stevenson'] }),
        resolvers: [{ id: 'unsure', resolve: async () => undefined }],
      },
      { name: 'announce', kind: 'code', run: (c) => `start ${(c.prior.pick as { choice: string }).choice}` },
    ],
  });
  const dir = await tmp();
  const a = await open(dir, build());
  const id = await startWorkflow(a.harness, a.root, build(), null); // the document exists once the run does
  const running = outcomeOf(a.harness, id).catch(() => undefined);
  const ask = await waitFor(a, (d) => d.steps[0].ask);
  assert.equal((ask as { question: string }).question, 'Who starts?');
  assert.equal((await doc(a)).steps[0].status, 'waiting');
  await a.close();
  await running;

  const b = await open(dir, build());
  assert.equal(await answerDecision(b.root, 'pick', 'Maybe', 'jf', ctx), false, 'only a listed option answers');
  assert.equal(await answerDecision(b.root, 'pick', 'Pollard', 'jf', ctx), true);
  assert.equal(await answerDecision(b.root, 'pick', 'Stevenson', 'jf', ctx), false, 'the first answer stands');
  const outcome = await runWorkflow(b.harness, b.root, build(), null);
  assert.equal(outcome.status === 'completed' && (outcome.result as { announce: string }).announce, 'start Pollard');
  await b.close();
});

test('a gated step waits for approval and runs its tool once with earlier results', async () => {
  const posted: string[] = [];
  const post = defineTool({
    name: 'post_comment',
    description: 'Post a comment',
    parameters: Type.Object({ body: Type.String() }),
    execute: async (args) => { posted.push(args.body); return { output: [{ type: 'text', text: 'posted' }] }; },
  });
  const workflow = defineWorkflow({
    name: 'gate',
    steps: [
      { name: 'draft', kind: 'code', run: () => 'LGTM' },
      { name: 'post', kind: 'gated', tool: post, args: (c) => ({ body: c.prior.draft as string }) },
    ],
  });
  const s = await open(await tmp(), workflow);
  const running = runWorkflow(s.harness, s.root, workflow, null);
  await pendingApprovals(s.root, ctx); // creates the document so it can be watched
  const watch = (await s.harness.watchDoc(Approvals, s.root.id, ctx))!;
  const id = await new Promise<string>((resolve) => {
    const find = () => Object.keys(watch.value?.items ?? {})[0];
    if (find()) return resolve(find());
    watch.start(async () => { if (find()) resolve(find()); });
  });
  await watch.stop();
  assert.deepEqual(posted, []);
  assert.equal(await resolveApproval(s.root, id, { decision: 'approve', by: 'jf' }, ctx), true);

  const outcome = await running;
  assert.equal(outcome.status, 'completed');
  assert.deepEqual(posted, ['LGTM']);
  await s.close();
});

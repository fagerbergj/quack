import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { contentText, createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { defineTool } from '@earendil-works/pi-durable';
import { obj } from '@earendil-works/pi-schema';
import { Artifacts, seal, type ArtifactKind } from '@quack/sdk';
import type { DispatchingExtension } from '@quack/sdk';
import { createSessionServer, openSession } from '@quack/session';
import { defineWorkflow, latestRun, outcomeOf, WorkflowDoc, workflowSessionExtensions } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-wf-')); dirs.push(d); return d; };

const note: ArtifactKind = { kind: 'test.note', path: 'note.md' };

const echo = defineWorkflow({
  name: 'echo',
  steps: [
    {
      name: 'write',
      kind: 'code',
      run: async (c) => {
        if (c.input === 'bad') throw new Error('boom');
        await c.api.env!.writeFile('note.md', String(c.input), ctx);
        await seal(c.api, note, ctx);
        return c.input;
      },
    },
  ],
});

function open(dir: string, extensions: DispatchingExtension[], ...replies: string[]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(replies.map((r) => fauxAssistantMessage(r)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
}

type Open = Awaited<ReturnType<typeof open>>;
const bound = (dir: string) => open(dir, workflowSessionExtensions({ workflow: 'echo' }, { echo }));
const doc = async (s: Open) => (await s.harness.snapshot(WorkflowDoc, s.root.id, ctx))!;

async function serve(session: Open) {
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  const post = (body: unknown) => fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify(body) }).then((r) => r.json() as Promise<Record<string, unknown>>);
  return { post, stop: () => new Promise((r) => server.close(r)) };
}

test('a bound agent runs its workflow on a submission and an unbound one still runs the loop', async () => {
  const b = await bound(await tmp());
  const http = await serve(b);
  const ran = await http.post({ input: 'ducks', wait: true });
  assert.equal(ran.status, 'done');
  assert.equal(ran.answer, '{"write":"ducks"}');
  await http.stop();
  await b.close();

  const u = await open(await tmp(), [], 'loop answer');
  const loop = await serve(u);
  assert.equal((await loop.post({ input: 'ducks', wait: true })).answer, 'loop answer');
  assert.equal(await u.harness.snapshot(WorkflowDoc, u.root.id, ctx), undefined);
  await loop.stop();
  await u.close();
});

test('a submission after a finished or failed run starts a fresh run in the same session', async () => {
  const s = await bound(await tmp());
  const http = await serve(s);
  const first = await http.post({ input: 'one', requestId: 'r1', wait: true });
  const firstTask = (await doc(s)).task;
  assert.equal((await http.post({ input: 'one', requestId: 'r1', wait: true })).submissionId, first.submissionId, 'the same request joins its run');

  const failed = await http.post({ input: 'bad', requestId: 'r2', wait: true });
  assert.equal(failed.status, 'unanswered');
  assert.equal(failed.detail, 'boom');
  const failedTask = (await doc(s)).task;
  assert.notEqual(failedTask, firstTask);

  const again = await http.post({ input: 'two', requestId: 'r3', wait: true });
  assert.equal(again.status, 'done');
  const after = await doc(s);
  assert.notEqual(after.task, failedTask);
  assert.deepEqual(after.steps.map((r) => r.status), ['done']);
  const versions = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['test.note'].versions;
  assert.deepEqual(versions.map((v) => v.content), ['one', 'two'], 'the new run sees the session it ran in');
  await http.stop();
  await s.close();
});

test('POST /resume reruns a failed run from its step, and answers 409 when nothing failed', async () => {
  let broken = true;
  const flaky = defineWorkflow({ name: 'flaky', steps: [{ name: 'only', kind: 'code', run: () => { if (broken) throw new Error('boom'); return 'ok'; } }] });
  const s = await open(await tmp(), workflowSessionExtensions({ workflow: 'flaky' }, { flaky }));
  const server = createSessionServer(s).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  const resume = () => fetch(`${base}/resume`, { method: 'POST' });

  assert.equal((await resume()).status, 409);
  await s.submit('go', undefined, true);
  broken = false;
  const res = await resume();
  assert.equal(res.status, 202);
  await outcomeOf(s.harness, ((await res.json()) as { submissionId: string }).submissionId);
  assert.equal((await doc(s)).steps[0].status, 'done');
  assert.equal((await resume()).status, 409);
  await new Promise((r) => server.close(r));
  await s.close();
});

test('chat tools work when a session restarts mid-chat-turn and resumes without dispatch', async () => {
  const dir = await tmp();
  const chatFlow = defineWorkflow({
    name: 'chat-echo',
    steps: [{ name: 'only', kind: 'code', run: async (c) => c.input }],
    chat: {
      prompt: 'Answer about the run.',
      tools: [defineTool({
        name: 'read_run',
        description: 'Read the latest run.',
        parameters: obj({}, { open: true }),
        async execute(_args, api, c) {
          const run = await latestRun(api, c);
          return { output: [{ type: 'text', text: JSON.stringify(run?.results ?? null) }] };
        },
      })],
    },
  });

  const openChat = (steps: FauxResponseStep[]) => {
    const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
    faux.setResponses(steps);
    const models = createModels();
    models.setProvider(faux.provider);
    const m = faux.getModel();
    return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: workflowSessionExtensions({ workflow: 'chat-echo' }, { 'chat-echo': chatFlow }) } });
  };

  let inFlight!: () => void;
  const started = new Promise<void>((r) => { inFlight = r; });
  const first = await openChat([
    (_c, opts) => new Promise((_res, rej) => {
      inFlight();
      opts?.signal?.addEventListener('abort', () => rej(new Error('aborted')), { once: true });
    }) as never,
  ]);
  const ran = await first.submit('ducks', 'start:1', true);
  assert.equal(ran.status, 'done');
  const firstTask = (await doc(first)).task;

  const chat = await first.submit('Check the run and rerun it', undefined, false);
  await started;
  await first.close();

  const seenResults: string[] = [];
  const resumed = await openChat([
    () => fauxAssistantMessage([fauxToolCall('read_run', {}), fauxToolCall('rerun_job', {})], { stopReason: 'toolUse' }),
    (c) => {
      for (const m of c.messages) if (m.role === 'toolResult') seenResults.push(contentText(m.content));
      return fauxAssistantMessage('done');
    },
  ]);
  const sub = await resumed.harness.submission(chat.submissionId as never, ctx);
  const settled = await sub!.wait(ctx);
  assert.equal(settled.status, 'done');
  assert.ok(seenResults.some((r) => r.includes('"only":"ducks"')), `expected read_run result, got: ${JSON.stringify(seenResults)}`);
  assert.ok(seenResults.some((r) => r.includes('Started run')), `expected rerun_job result, got: ${JSON.stringify(seenResults)}`);
  const secondTask = (await doc(resumed)).task!;
  assert.notEqual(secondTask, firstTask);
  assert.equal((await outcomeOf(resumed.harness, secondTask)).status, 'completed');
  await resumed.close();
});

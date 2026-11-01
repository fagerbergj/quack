import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { Artifacts, seal, type ArtifactKind } from '@quack/sdk';
import { createSessionServer, openSession, type DispatchingExtension } from '@quack/session';
import { defineWorkflow, WorkflowDoc, workflowSessionExtensions } from '../src/index.ts';

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

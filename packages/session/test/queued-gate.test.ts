import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool, GenerationTask, hook } from '@earendil-works/pi-durable';
import { gated, onWaiting, pendingApprovals, resolveApproval } from '@quack/sdk';
import { openSession } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

type Case = { decision: 'approve' | 'reject'; terminate?: boolean; yieldContinue?: boolean; restart?: boolean };

const toolOf = (terminate: boolean) => gated(defineTool({
  name: 'submit_contract', description: 'put a version up for approval', parameters: obj({}, { open: true }),
  execute: async () => ({ output: [{ type: 'text', text: 'approved' }], ...(terminate ? { control: { terminate: true } } : {}) }),
}));

// Like the contracts awaitingApproval hook: asks the run to go on once, which a queued follow-up must win over.
const continueOnce = () => {
  let asked = false;
  return hook(GenerationTask, { onYield: async () => (asked ? undefined : ((asked = true), { continue: 'Call submit_contract before you finish.' })) });
};

// Records the user messages of each model call; the first call parks on the gated tool, every later one answers.
function agentFor(c: Case, calls: string[][]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const respond = (context: { messages: Array<{ role: string; content: unknown }> }) => {
    calls.push(context.messages.filter((m) => m.role === 'user').map((m) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content))));
    return calls.length === 1 ? fauxAssistantMessage(fauxToolCall('submit_contract', {}), { stopReason: 'toolUse' }) : fauxAssistantMessage(`answer ${calls.length}`);
  };
  faux.setResponses(Array.from({ length: 8 }, () => respond) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const ext = defineExtension({ name: 'gate', tools: [toolOf(!!c.terminate)], hooks: c.yieldContinue ? [continueOnce()] : [] });
  return { models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [ext] } };
}

const parked = () => new Promise<void>((resolve) => { const off = onWaiting((n) => n > 0 && (off(), resolve())); });

const CASES: Array<[string, Case]> = [
  ['approved, the model answers after the tool', { decision: 'approve' }],
  ['rejected, the model answers after the tool', { decision: 'reject' }],
  ['approved, the tool ends the run', { decision: 'approve', terminate: true }],
  ['approved, a yield hook asks the run to continue', { decision: 'approve', yieldContinue: true }],
  ['approved after the process restarted while parked', { decision: 'approve', restart: true }],
];

for (const [label, c] of CASES) {
  test(`a follow-up queued behind a gated tool reaches the model exactly once: ${label}`, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'quack-queued-gate-'));
    dirs.push(dir);
    const calls: string[][] = [];
    let session = await openSession({ dir, ...agentFor(c, calls) });
    const waiting = parked();
    await session.submit('write the contract');
    await waiting;
    const queued = await session.submit('also cover pagination', undefined, false, 'followUp');
    assert.equal(queued.status, 'queued');
    assert.deepEqual((await session.queued()).map((q) => q.text), ['also cover pagination']);
    if (c.restart) {
      await session.close();
      const again = parked();
      session = await openSession({ dir, ...agentFor(c, calls) });
      await again;
      assert.deepEqual((await session.queued()).map((q) => q.text), ['also cover pagination'], 'the follow-up survived the restart');
    }
    const [approval] = await pendingApprovals(session.root, ctx);
    assert.ok(await resolveApproval(session.root, approval.id, { decision: c.decision, by: { kind: 'tailnet' } }, ctx));
    const settled = await (await session.harness.submission(queued.submissionId as never, ctx))!.wait(ctx);
    assert.equal(settled.status, 'done', `the follow-up was answered, not dropped: ${JSON.stringify(settled)}`);
    const took = calls.filter((users) => users.filter((u) => u.includes('also cover pagination')).length > 0);
    assert.ok(took.length > 0, `the follow-up reached the model: ${JSON.stringify(calls)}`);
    assert.ok(took.every((users) => users.filter((u) => u.includes('also cover pagination')).length === 1), `exactly once: ${JSON.stringify(calls)}`);
    assert.deepEqual(await session.queued(), []);
    await session.close();
  });
}

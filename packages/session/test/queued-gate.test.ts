import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool, GenerationTask, hook } from '@earendil-works/pi-durable';
import { Approvals, gated, onWaiting, pendingApprovals } from '@quack/sdk';
import { openSession } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

type Case = { yieldContinue?: boolean; legacy?: boolean };

const tool = gated(defineTool({
  name: 'submit_contract', description: 'put a version up for approval', parameters: obj({}, { open: true }),
  execute: async () => ({ output: [{ type: 'text', text: 'approved' }] }),
}));

// Like the contracts awaitingApproval hook: asks the run to go on once, which a queued follow-up must win over.
const continueOnce = () => {
  let asked = false;
  return hook(GenerationTask, { onYield: async () => (asked ? undefined : ((asked = true), { continue: 'Call submit_contract before you finish.' })) });
};

// Records the user and tool-result texts of each model call; the first call parks on the gated tool, every later one answers.
function agentFor(c: Case, calls: string[][]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const respond = (context: { messages: Array<{ role: string; content: unknown }> }) => {
    calls.push(context.messages.filter((m) => m.role !== 'assistant').map((m) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content))));
    return calls.length === 1 ? fauxAssistantMessage(fauxToolCall('submit_contract', {}), { stopReason: 'toolUse' }) : fauxAssistantMessage(`answer ${calls.length}`);
  };
  faux.setResponses(Array.from({ length: 8 }, () => respond) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const ext = defineExtension({ name: 'gate', tools: [tool], hooks: c.yieldContinue ? [continueOnce()] : [] });
  return { models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [ext] } };
}

const parked = () => new Promise<void>((resolve) => { const off = onWaiting((n) => n > 0 && (off(), resolve())); });
const TEXT = 'also cover pagination';

const CASES: Array<[string, Case]> = [
  ['the model answers after the not-run result', {}],
  ['a yield hook asks the run to continue', { yieldContinue: true }],
  // A row and a follow-up queued behind it, as code before #2397 left them: the rerun's gate applies the rule to its existing row.
  ['queued before an upgrade, the rerun after the restart withdraws its existing row', { legacy: true }],
];

for (const [label, c] of CASES) {
  test(`a follow-up queued behind a gated tool withdraws it and reaches the model exactly once, in the same run: ${label}`, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'quack-queued-gate-'));
    dirs.push(dir);
    const calls: string[][] = [];
    let session = await openSession({ dir, ...agentFor(c, calls) });
    const waiting = parked();
    await session.submit('write the contract');
    await waiting;
    const [{ id }] = await pendingApprovals(session.root, ctx);
    let submissionId: string;
    if (c.legacy) {
      submissionId = String((await session.root.submit({ type: 'input', content: TEXT, whenBusy: 'followUp' }, ctx)).id);
      assert.equal((await pendingApprovals(session.root, ctx)).length, 1);
      await session.close();
      session = await openSession({ dir, ...agentFor(c, calls) });
    } else {
      const queued = await session.submit(TEXT, undefined, false, 'followUp');
      submissionId = queued.submissionId;
      assert.deepEqual(await pendingApprovals(session.root, ctx), [], 'withdrawn in the commit that queued the follow-up');
    }
    const settled = await (await session.harness.submission(submissionId as never, ctx))!.wait(ctx);
    assert.equal(settled.status, 'done', `the follow-up was answered, not dropped: ${JSON.stringify(settled)}`);
    const took = calls.filter((texts) => texts.some((t) => t.includes(TEXT)));
    assert.ok(took.length > 0 && took.every((texts) => texts.filter((t) => t.includes(TEXT)).length === 1), `exactly once: ${JSON.stringify(calls)}`);
    assert.match(JSON.stringify(took[0]), /submit_contract was not run: the request was withdrawn: a person's message.*also cover pagination/, 'read in the turn after the not-run result');
    assert.equal((await session.harness.snapshot(Approvals, session.root.id, ctx))!.items[id]!.withdrawn?.reason, 'steered');
    assert.deepEqual(await session.queued(), []);
    await session.close();
  });
}

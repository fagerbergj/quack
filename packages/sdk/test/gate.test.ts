import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { gate, gated, resolveApproval, type Approval } from '../src/index.ts';

function setup(existing: Approval) {
  const doc = { items: { '1': existing } as Record<string, Approval> };
  let watched = 0;
  let stopped = 0;
  let ran = 0;
  const tool = gated({
    name: 'post_comment',
    description: 'post',
    parameters: {} as never,
    replay: 'safe',
    execute: async () => {
      ran++;
      return { output: [{ type: 'text', text: 'ok' }] };
    },
  });
  const api = {
    taskId: 1,
    now: () => 100,
    conversationId: 'c',
    commit: async <T>(fn: (tx: { conversation: () => Promise<undefined>; doc: () => Promise<typeof doc> }) => Promise<T>) => fn({ conversation: async () => undefined, doc: async () => doc }),
    watchDoc: async () => {
      watched++;
      return { value: doc, start: () => {}, stop: async () => { stopped++; } };
    },
  };
  return { tool, api, counts: () => ({ watched, stopped, ran }) };
}

test('gated does not open or leak a watchDoc subscription when an approval already has a verdict', async () => {
  const { tool, api, counts } = setup({
    tool: 'post_comment',
    args: { body: 'hi' },
    requestedAt: 1,
    verdict: { decision: 'approve', reason: null, by: 'alice', at: 2 },
    executing: false,
    done: false,
  });
  const res = await tool.execute({ body: 'hi' } as never, api as never, ctx);
  assert.equal(res.isError, undefined);
  assert.deepEqual(counts(), { watched: 0, stopped: 0, ran: 1 });
});

test('gated does not open or leak a watchDoc subscription when an approval is already withdrawn', async () => {
  const { tool, api, counts } = setup({
    tool: 'post_comment',
    args: { body: 'hi' },
    requestedAt: 1,
    verdict: null,
    executing: false,
    done: false,
    withdrawn: { at: 2, reason: 'superseded', supersededBy: '2' },
  });
  const res = await tool.execute({ body: 'hi' } as never, api as never, ctx);
  assert.equal(res.isError, true);
  assert.deepEqual(counts(), { watched: 0, stopped: 0, ran: 0 });
});

const asked = (over: Partial<Approval> = {}): Approval => ({ tool: 'post_comment', args: { body: 'hi' }, requestedAt: 1, verdict: null, executing: false, done: false, subject: { kind: 'k', version: 1 }, ...over });
const jf = { kind: 'person', name: 'jf' } as const;

test("a person's approve of a subject runs the approval gates first; a pass records it as the verdict and runs the tool", async (t) => {
  let saw: unknown;
  t.after(gate('approval.approving', async (e) => ((saw = e.data.id), { kind: 'pass', detail: { ok: true } })));
  const approving = { decision: 'approve' as const, reason: null, by: jf, at: 5 };
  const { tool, api, counts } = setup(asked({ approving }));
  const res = await tool.execute({ body: 'hi' } as never, api as never, ctx);
  assert.equal(res.isError, undefined);
  assert.deepEqual([saw, counts()], ['1', { watched: 0, stopped: 0, ran: 1 }]);
});

test("a gate that finds problems at approval.approving rejects the approval by the checks, with the findings, never by the approver", async (t) => {
  t.after(gate('approval.approving', async () => ({ kind: 'continue', feedback: 'v1 misses the intent' })));
  const { tool, api, counts } = setup(asked({ approving: { decision: 'approve', reason: null, by: jf, at: 5 } }));
  const res = await tool.execute({ body: 'hi' } as never, api as never, ctx);
  const row = (await api.watchDoc()).value.items['1']!;
  assert.deepEqual([row.verdict?.decision, row.verdict?.by, row.verdict?.reason, row.approving], ['reject', { kind: 'checks' }, 'v1 misses the intent', null]);
  assert.deepEqual([res.isError, (res.output?.[0] as { text: string }).text], [true, 'Not approved: the checks found problems:\nv1 misses the intent']);
  assert.equal(counts().ran, 0);
});

test('resolveApproval holds an approve of a subject for the gates, refuses a second one meanwhile, and reruns the checks over an old failure', async () => {
  const doc = { items: { a: asked(), b: asked({ checks: { at: 2, passed: false, decided: true, feedback: ['no'], detail: null } }), c: asked({ subject: null }) } as Record<string, Approval> };
  const conversation = { id: 'c', commit: async <T>(fn: (tx: { doc: () => Promise<typeof doc> }) => Promise<T>) => fn({ doc: async () => doc }) };
  const approve = (id: string) => resolveApproval(conversation as never, id, { decision: 'approve', by: jf }, ctx);
  assert.equal(await approve('a'), true);
  assert.deepEqual([doc.items.a.verdict, doc.items.a.approving?.decision], [null, 'approve']);
  assert.equal(await approve('a'), false, 'an approve being checked is not decided again');
  assert.equal(await resolveApproval(conversation as never, 'a', { decision: 'reject', reason: 'no', by: jf }, ctx), true, 'a reject still lands');
  assert.equal(await approve('b'), true);
  assert.deepEqual([doc.items.b.verdict, doc.items.b.approving?.decision], [null, 'approve'], 'a failure recorded before checks rejected is checked again');
  assert.equal(await approve('c'), true);
  assert.equal(doc.items.c.verdict?.decision, 'approve', 'an approval with no subject is recorded at once');
});

test('an approve after checks that could not run runs them again; one with a reason records it over them', async () => {
  const undecided = { at: 2, passed: false, decided: false, feedback: ['no usable answer'], detail: null };
  const doc = { items: { a: asked({ checks: undecided }), b: asked({ checks: undecided }) } as Record<string, Approval> };
  const conversation = { id: 'c', commit: async <T>(fn: (tx: { doc: () => Promise<typeof doc> }) => Promise<T>) => fn({ doc: async () => doc }) };
  assert.equal(await resolveApproval(conversation as never, 'a', { decision: 'approve', by: jf }, ctx), true);
  assert.deepEqual([doc.items.a.verdict, doc.items.a.approving?.decision], [null, 'approve'], 'a plain approve retries the checks');
  assert.equal(await resolveApproval(conversation as never, 'b', { decision: 'approve', reason: 'checked by hand', by: jf }, ctx), true);
  assert.deepEqual([doc.items.b.verdict?.decision, doc.items.b.verdict?.reason], ['approve', 'checked by hand']);
});

const failing = [
  ['denies', async () => ({ kind: 'deny' as const, reason: 'model unreachable' }), 'model unreachable'],
  ['throws', async () => { throw new Error('inputs unreadable'); }, 'The checks did not finish: inputs unreadable'],
] as const;
for (const [how, run, said] of failing) {
  test(`a gate that ${how} at approval.approving reopens the approval as undecided, held approve cleared and the call waiting again`, async (t) => {
    t.after(gate('approval.approving', run));
    const { tool, api, counts } = setup(asked({ approving: { decision: 'approve', reason: null, by: jf, at: 5 } }));
    void tool.execute({ body: 'hi' } as never, api as never, ctx);
    await new Promise((r) => setImmediate(r));
    const row = (await api.watchDoc()).value.items['1']!;
    assert.deepEqual([row.verdict, row.approving, row.checks?.decided, row.checks?.feedback], [null, null, false, [said]]);
    assert.deepEqual([counts().ran, counts().watched], [0, 2], 'not run, and watching for the next answer (beside this read)');
  });
}

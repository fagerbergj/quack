import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { InboxDoc } from '@earendil-works/pi-durable';
import { gated, type Approval } from '../src/index.ts';

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
    commit: async <T>(fn: (tx: { conversation: () => Promise<object>; doc: (token: unknown) => Promise<unknown> }) => Promise<T>) => fn({ conversation: async () => ({}), doc: async (token) => (token === InboxDoc ? { items: [] } : doc) }),
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

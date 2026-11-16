import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { setDefaultSessionTitle } from '../src/index.ts';
import { SessionTitle, type SessionTitleDoc } from '../src/title.ts';

// One call's view of the session: the title document a replay of the call finds as the crashed call left it.
function callApi(doc: SessionTitleDoc, now = 0) {
  const api = {
    conversationId: 'c', now: () => now,
    commit: async (change: (tx: unknown) => unknown) => change({ doc: async (token: unknown) => (token === SessionTitle ? doc : assert.fail('only the title doc')) }),
  };
  return api as unknown as ToolExecutionApi;
}

test('the title is folded, trimmed and cut to 120, and each change counts once', async () => {
  const doc: SessionTitleDoc = { title: null, changes: 0, at: 0 };
  await setDefaultSessionTitle(callApi(doc, 5), `  Sleeper\n schedule\tendpoint ${'x'.repeat(200)}`, ctx);
  assert.equal(doc.title, `Sleeper schedule endpoint ${'x'.repeat(94)}`);
  assert.deepEqual([doc.title!.length, doc.changes, doc.at], [120, 1, 5]);
  await setDefaultSessionTitle(callApi(doc, 9), 'Sleeper schedule, retitled', ctx);
  assert.deepEqual(doc, { title: 'Sleeper schedule, retitled', changes: 2, at: 9 }, 'last write wins');
});

test('a replay of the calling step with the same title, and an empty title, change nothing', async () => {
  const doc: SessionTitleDoc = { title: null, changes: 0, at: 0 };
  await setDefaultSessionTitle(callApi(doc, 5), 'Sleeper schedule endpoint', ctx);
  // The runtime reruns a replay-safe call whose result was not committed; its write is already in the document.
  await setDefaultSessionTitle(callApi(doc, 7), 'Sleeper  schedule endpoint ', ctx);
  await setDefaultSessionTitle(callApi(doc, 8), ' \n ', ctx);
  assert.deepEqual(doc, { title: 'Sleeper schedule endpoint', changes: 1, at: 5 });
});

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import { evalService, manifest, type PullRequestRecord } from '../src/index.ts';

test('the review adapter blocks on request_changes only, and serves the pinned pull request', () => {
  const [adapter] = manifest.evalAdapters!;
  assert.equal(adapter.kind, 'github.review');
  assert.deepEqual(Object.keys(adapter.services!), ['github.pr']);
  assert.equal(adapter.blocks!('## Verdict\nrequest_changes\n'), true);
  assert.equal(adapter.blocks!('## Verdict\napprove\n'), false);
});

const pr: PullRequestRecord = { owner: 'acme', repo: 'widgets', number: 7, title: 't', body: '', author: 'a', state: 'open', headSha: 'abc', headRef: 'f', baseRef: 'main', checks: [], linkedIssues: [] };

test('a replay is served the pinned pull request, and its reviews are captured, never posted', async () => {
  const svc = await evalService({ pr });
  const url = svc.env.QUACK_SESSION_LISTENER_URL;
  const auth = { authorization: `Bearer ${await readFile(svc.env.QUACK_LAUNCH_TOKEN_FILE, 'utf8')}` };
  try {
    assert.deepEqual(await (await fetch(`${url}/apps/github/pulls/acme/widgets/7`, { headers: auth })).json(), pr);
    assert.deepEqual(await (await fetch(`${url}/apps/github/pulls/acme/widgets/7/head`, { headers: auth })).json(), { headSha: 'abc', body: '' }, 'the head submit_review checks before posting');
    assert.equal((await fetch(`${url}/pulls/acme/widgets/8`, { headers: auth })).status, 404, 'only the pinned PR exists');
    assert.equal((await fetch(`${url}/git-credential?url=https://github.com/acme/widgets`, { headers: auth })).status, 404, 'no credentials, so nothing can be pushed');
    assert.equal((await fetch(`${url}/pulls/acme/widgets/7`)).status, 401);
    const post = (r: Record<string, unknown>) => fetch(`${url}/reviews`, { method: 'POST', headers: auth, body: JSON.stringify({ owner: 'acme', repo: 'widgets', number: 7, commitId: 'abc', event: 'APPROVE', body: 'ok', ...r }) });
    const posted = await post({});
    assert.deepEqual([posted.status, await posted.json()], [201, { url: '(eval: not delivered)' }]);
    assert.equal((await post({ number: 8 })).status, 404, 'a review of another PR is not a delivery');
    assert.equal((await post({ commitId: 'def' })).status, 404, 'nor is a review of another head');
    assert.deepEqual(svc.captured, [{ owner: 'acme', repo: 'widgets', number: 7, commitId: 'abc', event: 'APPROVE', body: 'ok' }]);
  } finally {
    await svc.close();
  }
});

test('a pin recorded after its PR merged is still served open, so the replay reviews it', async () => {
  const svc = await evalService({ pr: { ...pr, state: 'closed' } });
  try {
    const served = await (await fetch(`${svc.env.QUACK_SESSION_LISTENER_URL}/apps/github/pulls/acme/widgets/7`, { headers: { authorization: `Bearer ${await readFile(svc.env.QUACK_LAUNCH_TOKEN_FILE, 'utf8')}` } })).json();
    assert.equal(served.state, 'open');
  } finally {
    await svc.close();
  }
});

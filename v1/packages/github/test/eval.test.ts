import assert from 'node:assert/strict';
import { test } from 'node:test';
import { evalService, type PullRequestRecord } from '../src/index.ts';

const pr: PullRequestRecord = { owner: 'acme', repo: 'widgets', number: 7, title: 't', body: '', author: 'a', headSha: 'abc', headRef: 'f', baseRef: 'main', checks: [], linkedIssues: [] };

test('a replay is served the pinned pull request, and its reviews are captured, never posted', async () => {
  const svc = await evalService({ pr });
  const url = svc.env.QUACK_GH_SERVICE_URL;
  const auth = { authorization: `Bearer ${svc.env.QUACK_GH_SERVICE_TOKEN}` };
  try {
    assert.deepEqual(await (await fetch(`${url}/pulls/acme/widgets/7`, { headers: auth })).json(), pr);
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

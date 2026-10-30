import assert from 'node:assert/strict';
import { execFile, execFileSync } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension } from '@earendil-works/pi-durable';
import { openSession } from '@quack/session';
import { checkoutTool, PullRequestDoc } from '../src/index.ts';

const dirs: string[] = [];
const servers: Server[] = [];
after(async () => {
  servers.forEach((s) => s.close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});
const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-gh-')); dirs.push(d); return d; };
const sh = (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, encoding: 'utf8', env: { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t' } }).trim();

// A local "github.com": a bare acme/widgets.git with main and refs/pull/7/head, like GitHub serves.
async function remotes() {
  const base = await tmp();
  const work = await tmp();
  sh(work, 'init', '-q', '-b', 'main');
  await writeFile(join(work, 'AGENTS.md'), '# Conventions\nComments stay at two lines.\n');
  await writeFile(join(work, 'app.ts'), 'export const a = 1;\n');
  sh(work, 'add', '.');
  sh(work, 'commit', '-qm', 'base');
  await writeFile(join(work, 'app.ts'), 'export const a = 2;\n');
  sh(work, 'commit', '-qam', 'change');
  const head = sh(work, 'rev-parse', 'HEAD');
  sh(base, 'init', '-q', '--bare', 'acme/widgets.git');
  sh(work, 'push', '-q', join(base, 'acme/widgets.git'), 'HEAD~1:refs/heads/main', `${head}:refs/pull/7/head`);
  return { base: `file://${base}`, head };
}

async function fakeService(head: string) {
  const server = createServer((req, res) => {
    if (req.headers.authorization !== 'Bearer svc') return res.writeHead(401).end('{}');
    if (req.url?.startsWith('/git-credential')) return res.writeHead(200).end(JSON.stringify({ username: 'x-access-token', token: 'ghs_from_service' }));
    const pr = { owner: 'acme', repo: 'widgets', number: 7, title: 'Bump a', body: 'Fixes #3', author: 'dev', headSha: head, headRef: 'bump', baseRef: 'main', checks: [{ name: 'ci', status: 'completed', conclusion: 'success' }], linkedIssues: [{ number: 3, title: 'a should be 2', body: 'It is 1.' }] };
    res.writeHead(200).end(JSON.stringify(pr));
  });
  servers.push(server.listen(0));
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

test('checks out the PR head with its base, records the PR, and is safe to call again', async () => {
  const { base, head } = await remotes();
  const serviceUrl = await fakeService(head);
  let result = '';
  const faux = fauxProvider({ models: [{ id: 'w' }] });
  const call = () => fauxAssistantMessage(fauxToolCall('checkout_pr', { repository: 'acme/widgets', number: 7 }), { stopReason: 'toolUse' });
  faux.setResponses([call(), call(), (c) => { result = JSON.stringify(c.messages.at(-1)); return fauxAssistantMessage('Checked out.'); }]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const tool = checkoutTool({ serviceUrl, serviceToken: 'svc', gitBaseUrl: base });
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'gh', tools: [tool] })] } });

  const done = await session.submit('Review acme/widgets#7', 'co-1', true);
  assert.equal(done.status === 'done' && done.answer, 'Checked out.');
  assert.equal(sh(join(session.workspace, 'repo'), 'rev-parse', 'HEAD'), head);
  assert.match(sh(join(session.workspace, 'repo'), 'diff', 'origin/main...HEAD'), /-export const a = 1;\n\+export const a = 2;/);
  assert.match(result, /Fixes #3/);
  assert.match(result, /CI checks: ci=success/);
  assert.match(result, /Linked issue #3: a should be 2/);
  assert.match(result, /Comments stay at two lines/, 'conventions from AGENTS.md');
  const doc = await session.harness.snapshot(PullRequestDoc, session.root.id, BACKGROUND_CONTEXT);
  assert.equal(doc!.pr!.headSha, head);
  await session.close();
});

test('the askpass helper answers git with a fresh token from the service', async () => {
  const serviceUrl = await fakeService('x');
  const askpass = fileURLToPath(new URL('../src/askpass.mjs', import.meta.url));
  const env = { ...process.env, QUACK_GH_SERVICE_URL: serviceUrl, QUACK_GH_SERVICE_TOKEN: 'svc', QUACK_GH_REMOTE: 'https://github.com/acme/widgets.git' };
  // Async: the fake service runs in this process, so a blocking spawn would deadlock against it.
  const run = async (prompt: string, extra = {}) => (await promisify(execFile)(askpass, [prompt], { env: { ...env, ...extra }, encoding: 'utf8' })).stdout.trim();
  assert.equal(await run("Username for 'https://github.com': "), 'x-access-token');
  assert.equal(await run("Password for 'https://x-access-token@github.com': "), 'ghs_from_service');
  await assert.rejects(run('Password: ', { QUACK_GH_SERVICE_TOKEN: 'wrong' }));
});

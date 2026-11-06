import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { eachItem, stopChild, untilSettled } from '../src/index.ts';

// Answers each POST with the next scripted [status, body]; 'drop' destroys the connection instead.
async function runtimeStub(answers: Array<[number, unknown] | 'drop'>) {
  const ids: string[] = [];
  const server = createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    ids.push(JSON.parse(raw).requestId);
    const next = answers.length > 1 ? answers.shift()! : answers[0];
    if (next === 'drop') return res.destroy();
    res.writeHead(next[0], { 'content-type': 'application/json' }).end(JSON.stringify(next[1]));
  }).listen(0, '127.0.0.1');
  await once(server, 'listening');
  return { ids, url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, close: () => (server.closeAllConnections(), server.close()) };
}

const inv = { input: 'go', requestId: 'r-1' };
const later = () => Date.now() + 60_000;

test('a run is polled by its request id until it settles, and a dropped connection is retried', async () => {
  const s = await runtimeStub(['drop', [202, { status: 'queued' }], [202, { status: 'placed' }], [202, { status: 'done', answer: 'ok' }]]);
  assert.deepEqual(await untilSettled({ url: s.url }, inv, later(), undefined, 0), { status: 'done', answer: 'ok' });
  assert.deepEqual(s.ids, ['r-1', 'r-1', 'r-1', 'r-1']);
  s.close();
});

test('an error status from /invocations is a harness failure, not an unanswered run', async () => {
  for (const [status, body, message] of [[500, { error: 'boom' }, /HTTP 500: boom/], [400, { error: 'input must be a string' }, /HTTP 400/], [200, {}, /without a status/]] as const) {
    const s = await runtimeStub([[status, body]]);
    await assert.rejects(untilSettled({ url: s.url }, inv, later(), undefined, 0), message);
    s.close();
  }
});

test('a run that never settles ends at the deadline', async () => {
  const s = await runtimeStub([[202, { status: 'queued' }]]);
  await assert.rejects(untilSettled({ url: s.url }, inv, Date.now(), undefined, 0), /run timed out/);
  s.close();
});

test('a child that ignores SIGTERM is killed', async () => {
  const child = spawn(process.execPath, ['-e', "process.on('SIGTERM', () => {}); console.log('ready'); setInterval(() => {}, 1000)"], { stdio: ['ignore', 'pipe', 'ignore'] });
  await once(child.stdout!, 'data');
  await stopChild(child, 0);
  assert.equal(child.signalCode, 'SIGKILL');
  await stopChild(child, 0);
});

test('one failing item is reported while its siblings keep their results', async () => {
  const { results, failures } = await eachItem([1, 2, 3, 4], 2, async (n) => {
    if (n === 2) throw new Error('publish 503');
    return n * 10;
  });
  assert.deepEqual(results.sort(), [10, 30, 40]);
  assert.deepEqual(failures.map((f) => [f.item, f.error.message]), [[2, 'publish 503']]);
});

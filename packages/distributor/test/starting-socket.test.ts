import assert from 'node:assert/strict';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { nextFrame } from '../../session/test/ws-helpers.ts';
import { createDistributor, processDriver, sessionHost, type HostDriver, type Launch } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const KEY = 'research:stopped';
const echo = (dataDir: string) => processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], dataDir, defaultAgent: '' });

// A session with one answered run on disk and no process, as most of the Sessions list is.
async function stoppedSession() {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-starting-'));
  dirs.push(dataDir);
  const driver = echo(dataDir);
  const url = await driver.ensure(KEY);
  const res = await fetch(`${url}/invocations`, { method: 'POST', headers: { authorization: `Bearer ${driver.credential(KEY)}`, 'content-type': 'application/json' }, body: JSON.stringify({ input: 'first', wait: true }) });
  assert.equal(res.status, 200);
  await driver.stop(KEY);
  return dataDir;
}

async function serve(driver: HostDriver, timers: Array<() => void> = []) {
  const server = createDistributor(driver, { idleMs: 60_000, setTimer: (fn) => (timers.push(fn), () => {}) }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const close = async () => {
    await Promise.all(driver.running().map((k) => driver.stop(k)));
    await new Promise((r) => server.close(r));
  };
  return { url: `ws://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent(KEY)}/ws`, close };
}

const text = (frame: { items?: unknown }) => JSON.stringify(frame.items ?? []);

test('a page on a stopped session gets its history while the process has not started', async () => {
  const dataDir = await stoppedSession();
  const never: Launch = () => new Promise(() => {});
  const { url, close } = await serve(sessionHost({ dataDir, defaultAgent: '' }, never));
  const ws = new WebSocket(url);
  try {
    const view = await nextFrame(ws, (f) => f.type === 'view');
    assert.match(text(view), /echo: first/);
  } finally {
    ws.close();
    await close();
  }
});

test('sends to a stopped session are accepted and queued at once, then each is delivered once, in order', async () => {
  const dataDir = await stoppedSession();
  const driver = echo(dataDir);
  const { url, close } = await serve(driver);
  const ws = new WebSocket(url);
  try {
    await nextFrame(ws, (f) => f.type === 'view');
    const queued = nextFrame(ws, (f) => f.type === 'queued' && f.items.length === 2);
    ws.send(JSON.stringify({ type: 'input', input: 'second', requestId: 'r-2' }));
    ws.send(JSON.stringify({ type: 'input', input: 'third', requestId: 'r-3', whenBusy: 'followUp' }));
    assert.deepEqual((await queued).items.map((i: { text: string }) => i.text), ['second', 'third']);
    let items = '';
    const answered = (word: string) => nextFrame(ws, (f) => f.type === 'view' && ((items = f.items ? text(f) : items), items.includes(`echo: ${word}`) && !f.live.run));
    await answered('third');
    assert.deepEqual(driver.running(), [KEY]);
    // Once the queue is delivered the page talks to the process itself.
    ws.send(JSON.stringify({ type: 'input', input: 'fourth', requestId: 'r-4' }));
    await answered('fourth');
    assert.deepEqual(items.match(/echo: \w+/g), ['echo: first', 'echo: second', 'echo: third', 'echo: fourth']);
  } finally {
    ws.close();
    await close();
  }
});

test('a session that cannot start keeps the queued send on the page beside the error, and the start is retried', async () => {
  const dataDir = await stoppedSession();
  let launches = 0;
  const failing: Launch = async () => { launches++; throw new Error('no capacity'); };
  const timers: Array<() => void> = [];
  const { url, close } = await serve(sessionHost({ dataDir, defaultAgent: '' }, failing), timers);
  const ws = new WebSocket(url);
  try {
    assert.match((await nextFrame(ws, (f) => f.type === 'error')).error, /did not start: no capacity/);
    const accepted = nextFrame(ws, (f) => f.type === 'accepted' && f.requestId === 'r-1');
    ws.send(JSON.stringify({ type: 'input', input: 'are you there', requestId: 'r-1' }));
    await accepted;
    const failed = nextFrame(ws, (f) => f.type === 'error');
    timers.splice(0).forEach((fire) => fire());
    assert.match((await failed).error, /no capacity; your queued messages are kept; retrying/);
    assert.equal(launches, 2, 'the retry started it again');
    const shown = nextFrame(ws, (f) => f.type === 'queued');
    ws.send(JSON.stringify({ type: 'earlier' }));
    ws.send(JSON.stringify({ type: 'withdraw', id: 'none' }));
    ws.send(JSON.stringify({ type: 'input', input: 'still?', requestId: 'r-2' }));
    assert.deepEqual((await shown).items.map((i: { text: string }) => i.text), ['are you there', 'still?']);
  } finally {
    ws.close();
    await close();
  }
});

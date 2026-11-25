import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { WebSocketServer, type WebSocket as ServerSocket } from 'ws';
import { nextFrame } from '../../session/test/ws-helpers.ts';
import { createDistributor, openOutbox, processDriver, sessionHost, type HostDriver, type Launch, type Outbox } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const KEY = 'research:stopped';
const echo = (dataDir: string) => processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], dataDir, defaultAgent: '' });
const never: Launch = () => new Promise(() => {});
const input = (text: string, requestId: string, extra: object = {}) => JSON.stringify({ type: 'input', input: text, requestId, ...extra });
const texts = (frame: { items: Array<{ text: string }> }) => frame.items.map((i) => i.text);

async function tempDir(prefix: string) {
  const dir = await mkdtemp(join(tmpdir(), prefix));
  dirs.push(dir);
  return dir;
}

// A session with one answered run on disk and no process, as most of the Sessions list is.
async function stoppedSession() {
  const dataDir = await tempDir('quack-starting-');
  const driver = echo(dataDir);
  const url = await driver.ensure(KEY);
  const res = await fetch(`${url}/invocations`, { method: 'POST', headers: { authorization: `Bearer ${driver.credential(KEY)}`, 'content-type': 'application/json' }, body: JSON.stringify({ input: 'first', wait: true }) });
  assert.equal(res.status, 200);
  await driver.stop(KEY);
  return dataDir;
}

// Timers are collected, never run on their own: a test fires a retry when it means to.
async function serve(driver: HostDriver, o: { timers?: Array<() => void>; outbox?: Outbox } = {}) {
  const server = createDistributor(driver, { idleMs: 60_000, outbox: o.outbox, setTimer: (fn) => (o.timers?.push(fn), () => {}) }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const url = `ws://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent(KEY)}/ws`;
  const pages: WebSocket[] = [];
  const page = () => {
    const ws = new WebSocket(url);
    pages.push(ws);
    return ws;
  };
  const close = async () => {
    pages.forEach((p) => p.close());
    await Promise.all(driver.running().map((k) => driver.stop(k)));
    await new Promise((r) => server.close(r));
  };
  return { page, close };
}

// Collects a page's transcript text across view frames, which carry items only when they change.
function transcript(ws: WebSocket) {
  let items = '';
  return (word: string) => nextFrame(ws, (f) => f.type === 'view' && ((items = f.items ? JSON.stringify(f.items) : items), items.includes(`echo: ${word}`) && !f.live.run)).then(() => items.match(/echo: \w+/g));
}

test('a page on a stopped session opens on its recorded frames while the process has not started', async () => {
  const dataDir = await stoppedSession();
  const { page, close } = await serve(sessionHost({ dataDir, defaultAgent: '' }, never));
  const ws = page();
  const seen: Record<string, any> = {};
  try {
    await nextFrame(ws, (f) => ((seen[f.type] = f), ['view', 'queued', 'standing', 'artifacts'].every((t) => t in seen)));
    assert.match(JSON.stringify(seen.view.items), /echo: first/);
    assert.deepEqual(seen.queued.items, []);
    assert.deepEqual(seen.standing, { type: 'standing', questions: [], failures: [], approvals: [] });
    assert.deepEqual(seen.artifacts.artifacts, []);
  } finally {
    await close();
  }
});

test('sends to a stopped session are accepted and queued at once, then each is delivered once, in order', async () => {
  const dataDir = await stoppedSession();
  const driver = echo(dataDir);
  const { page, close } = await serve(driver);
  const ws = page();
  try {
    await nextFrame(ws, (f) => f.type === 'view');
    const queued = nextFrame(ws, (f) => f.type === 'queued' && f.items.length === 2);
    ws.send(input('second', 'r-2'));
    ws.send(input('third', 'r-3', { whenBusy: 'followUp' }));
    assert.deepEqual(texts(await queued), ['second', 'third']);
    const answered = transcript(ws);
    await answered('third');
    assert.deepEqual(driver.running(), [KEY]);
    // Once the queue is delivered the page talks to the process itself.
    ws.send(input('fourth', 'r-4'));
    assert.deepEqual(await answered('fourth'), ['echo: first', 'echo: second', 'echo: third', 'echo: fourth']);
  } finally {
    await close();
  }
});

test('a session that cannot start keeps a queued send beside the error for every page, and retries', async () => {
  const dataDir = await stoppedSession();
  let launches = 0;
  const failing: Launch = async () => { launches++; throw new Error('no capacity'); };
  const timers: Array<() => void> = [];
  const { page, close } = await serve(sessionHost({ dataDir, defaultAgent: '' }, failing), { timers });
  const ws = page();
  try {
    assert.match((await nextFrame(ws, (f) => f.type === 'error')).error, /did not start: no capacity; retrying/);
    const accepted = nextFrame(ws, (f) => f.type === 'accepted' && f.requestId === 'r-1');
    ws.send(input('are you there', 'r-1'));
    await accepted;
    const failed = nextFrame(ws, (f) => f.type === 'error' && /queued messages are kept/.test(f.error));
    timers.splice(0).forEach((fire) => fire());
    assert.match((await failed).error, /no capacity; your queued messages are kept; retrying/);
    assert.ok(launches >= 2, 'the retry started it again');
    // A reload, or another device, sees what is waiting.
    const other = page();
    assert.deepEqual(texts(await nextFrame(other, (f) => f.type === 'queued')), ['are you there']);
  } finally {
    await close();
  }
});

test('queued sends outlive the distributor: the next one delivers them as it starts', async () => {
  const dataDir = await stoppedSession();
  const path = join(await tempDir('quack-outbox-'), 'outbox.sqlite');
  const first = await serve(sessionHost({ dataDir, defaultAgent: '' }, never), { outbox: openOutbox(path) });
  const ws = first.page();
  await nextFrame(ws, (f) => f.type === 'view');
  const accepted = nextFrame(ws, (f) => f.type === 'accepted');
  ws.send(input('still there', 'r-1'));
  await accepted;
  await first.close();

  const driver = echo(dataDir);
  const second = await serve(driver, { outbox: openOutbox(path) });
  try {
    assert.deepEqual(await transcript(second.page())('still'), ['echo: first', 'echo: still']);
  } finally {
    await second.close();
  }
});

test('a queued send is withdrawn before delivery; Send now keeps it queued with a note; an over-limit send is refused', async () => {
  const dataDir = await stoppedSession();
  const { page, close } = await serve(sessionHost({ dataDir, defaultAgent: '' }, never), { outbox: openOutbox(':memory:', { inputs: 2, bytes: 1 << 20 }) });
  const ws = page();
  try {
    await nextFrame(ws, (f) => f.type === 'view');
    const two = nextFrame(ws, (f) => f.type === 'queued' && f.items.length === 2);
    ws.send(input('keep', 'r-1'));
    ws.send(input('drop', 'r-2'));
    const [, drop] = (await two).items;
    const over = nextFrame(ws, (f) => f.type === 'error' && f.requestId === 'r-3');
    ws.send(input('too many', 'r-3'));
    assert.match((await over).error, /at most 2 messages/);
    const note = nextFrame(ws, (f) => f.type === 'error' && !f.requestId);
    ws.send(JSON.stringify({ type: 'steer', id: drop.id }));
    assert.match((await note).error, /as soon as it starts/);
    const one = nextFrame(ws, (f) => f.type === 'queued');
    ws.send(JSON.stringify({ type: 'withdraw', id: drop.id }));
    assert.deepEqual(texts(await one), ['keep']);
  } finally {
    await close();
  }
});

test('a withdraw that races delivery is refused, and the row leaves every page once the session takes it', async () => {
  const dataDir = await stoppedSession();
  // A stand-in session that answers an input only when the test says so.
  const http = createServer().listen(0);
  await new Promise((r) => http.once('listening', r));
  const wss = new WebSocketServer({ server: http });
  const inputs: Array<{ ws: ServerSocket; requestId: string }> = [];
  let arrived = () => {};
  wss.on('connection', (s) => s.on('message', (d) => {
    const frame = JSON.parse(String(d));
    if (frame.type === 'input') (inputs.push({ ws: s, requestId: frame.requestId }), arrived());
  }));
  const fake: Launch = async () => ({ url: `http://127.0.0.1:${(http.address() as AddressInfo).port}`, token: 't', stop: async () => {}, onExit: () => {} });
  const { page, close } = await serve(sessionHost({ dataDir, defaultAgent: '' }, fake));
  const ws = page();
  try {
    await nextFrame(ws, (f) => f.type === 'view');
    const sent = new Promise<void>((r) => (arrived = r));
    const shown = nextFrame(ws, (f) => f.type === 'queued' && f.items.length === 1);
    ws.send(input('on its way', 'r-1'));
    const [row] = (await shown).items;
    await sent;
    const refused = nextFrame(ws, (f) => f.type === 'error');
    ws.send(JSON.stringify({ type: 'withdraw', id: row.id }));
    assert.match((await refused).error, /already on its way/);
    const gone = nextFrame(ws, (f) => f.type === 'queued' && f.items.length === 0);
    inputs[0]!.ws.send(JSON.stringify({ type: 'accepted', requestId: 'r-1' }));
    await gone;
    assert.equal(inputs.length, 1, 'delivered once');
  } finally {
    await close();
    wss.close();
    await new Promise((r) => http.close(r));
  }
});

import assert from 'node:assert/strict';
import { createServer, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test, type TestContext } from 'node:test';
import { pullEventStream } from '../src/events.ts';
import type { PullEvent } from '../src/pulls.ts';

// Each test gets its own stream, server and fake clock: the 20-stream test would otherwise fill the first one's subscribe log.
async function harness(t: TestContext) {
  const listeners = new Set<(e: PullEvent) => void>();
  let current: ServerResponse | undefined;
  const headersAtSubscribe: boolean[] = [];
  const subscribe = (fn: (e: PullEvent) => void) => {
    headersAtSubscribe.push(current!.headersSent);
    listeners.add(fn);
    return () => void listeners.delete(fn);
  };
  // A fake clock: the keepalive fires only when a test ticks it.
  const beats = new Map<number, () => void>();
  let ids = 0;
  const timers = { setInterval: (fn: () => void) => (beats.set(++ids, fn), ids), clearInterval: (id: number) => void beats.delete(id) };
  const stream = pullEventStream(subscribe, timers);
  const server = createServer((req, res) => ((current = res), stream.handle(req, res))).listen(0);
  await new Promise((r) => server.once('listening', r));
  t.after(() => (stream.stop(), server.closeAllConnections(), server.close()));
  return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}/events`, listeners, headersAtSubscribe, beats, stream };
}

async function readUntil(reader: ReadableStreamDefaultReader<Uint8Array>, want: RegExp) {
  let text = '';
  while (!want.test(text)) text += new TextDecoder().decode((await reader.read()).value);
  return text;
}
const event: PullEvent = { action: 'closed', repository: { full_name: 'acme/widgets' }, pull_request: { number: 7, state: 'closed', merged: true, merged_at: 'm', closed_at: 'm', html_url: 'h', title: 't' } };

test('a stream subscribes before its headers, sends each event as pull_request, and a keepalive on each tick', async (t) => {
  const { url, listeners, headersAtSubscribe, beats, stream } = await harness(t);
  const res = await fetch(url);
  assert.deepEqual([res.status, res.headers.get('content-type'), res.headers.get('cache-control'), res.headers.get('x-accel-buffering')], [200, 'text/event-stream', 'no-cache, no-transform', 'no']);
  assert.deepEqual(headersAtSubscribe, [false]);
  const reader = res.body!.getReader();
  await readUntil(reader, /^: keepalive\n\n$/);
  for (const fn of listeners) fn(event);
  assert.equal(await readUntil(reader, /\n\n$/), `event: pull_request\ndata: ${JSON.stringify(event)}\n\n`);
  [...beats.values()].forEach((beat) => beat());
  assert.equal(await readUntil(reader, /\n\n$/), ': keepalive\n\n');
  stream.stop();
  assert.equal((await reader.read()).done, true);
  assert.deepEqual([listeners.size, beats.size], [0, 0], 'stop unsubscribes and clears the keepalive');
});

test('past 20 streams the next is a 503 problem with Retry-After 30; stop ends every stream', async (t) => {
  const { url, stream } = await harness(t);
  const readers = await Promise.all(Array.from({ length: 20 }, async () => (await fetch(url)).body!.getReader()));
  const refused = await fetch(url);
  assert.deepEqual([refused.status, refused.headers.get('retry-after'), refused.headers.get('content-type')], [503, '30', 'application/problem+json']);
  stream.stop();
  const ends = await Promise.all(readers.map(async (r) => { while (!(await r.read()).done); return true; }));
  assert.equal(ends.length, 20);
  assert.equal((await fetch(url)).status, 200, 'stopped streams free their slots');
  stream.stop();
});

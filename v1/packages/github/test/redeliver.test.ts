import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';
import { mkdtempSync } from 'node:fs';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileStore, githubApp, redeliverFailed } from '../src/index.ts';
import { failedDeliveries } from '../src/redeliver.ts';

const { privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048, privateKeyEncoding: { type: 'pkcs8', format: 'pem' }, publicKeyEncoding: { type: 'spki', format: 'pem' } });
const NOW = Date.parse('2026-11-01T08:00:00Z');
const d = (id: number, guid: string, minAgo: number, status_code: number, redelivery = false) => ({ id, guid, delivered_at: new Date(NOW - minAgo * 60_000).toISOString(), redelivery, status_code });

// A fake GitHub: serves the delivery pages (Link header between them) and records redelivery requests.
async function fakeGithub(pages: any[][], failPost = false) {
  const posts: string[] = [];
  const server = createServer((req, res) => {
    if (req.method === 'POST') {
      posts.push(req.url!);
      return failPost ? res.writeHead(500).end('{}') : res.writeHead(202).end('{}');
    }
    const cursor = Number(new URL(req.url!, 'http://x').searchParams.get('cursor') ?? 0);
    const headers: Record<string, string> = { 'content-type': 'application/json' };
    if (cursor + 1 < pages.length) headers.link = `<https://api.github.com/app/hook/deliveries?per_page=100&cursor=${cursor + 1}>; rel="next"`;
    res.writeHead(200, headers).end(JSON.stringify(pages[cursor]));
  });
  server.listen(0);
  await new Promise((r) => server.once('listening', r));
  const app = githubApp({ appId: '123', privateKey, apiUrl: `http://127.0.0.1:${(server.address() as AddressInfo).port}` });
  return { app, posts, close: () => server.close() };
}

const opts = { now: () => NOW, gapMs: 0 };
const memory = (since?: number) => ({ get since() { return since; }, read: () => since, write(m: number) { since = m; } });

test('a failed delivery is redelivered once and a succeeded one is not', async () => {
  const gh = await fakeGithub([[d(1, 'a', 5, 503), d(2, 'b', 4, 200), d(3, 'c', 3, 0), d(4, 'c', 2, 200, true)]]);
  const store = memory();
  assert.equal(await redeliverFailed(gh.app, store, opts), 1);
  assert.deepEqual(gh.posts, ['/app/hook/deliveries/1/attempts']);
  assert.equal(await redeliverFailed(gh.app, store, opts), 0, 'the mark moved past it');
  gh.close();
});

test('the high-water mark survives a restart', async () => {
  const file = join(mkdtempSync(join(tmpdir(), 'redeliver-')), 'state.json');
  const gh = await fakeGithub([[d(1, 'a', 5, 503)]]);
  await redeliverFailed(gh.app, fileStore(file), opts);
  assert.equal(await redeliverFailed(gh.app, fileStore(file), opts), 0);
  assert.equal(gh.posts.length, 1);
  gh.close();
});

test('pages are followed back to the window; older failures are left alone', async () => {
  const gh = await fakeGithub([[d(5, 'e', 1, 503)], [d(1, 'a', 20, 503), d(2, 'old', 40, 503)]]);
  assert.equal(await redeliverFailed(gh.app, memory(), opts), 2);
  assert.deepEqual(gh.posts.sort(), ['/app/hook/deliveries/1/attempts', '/app/hook/deliveries/5/attempts']);
  gh.close();
});

test('a failed re-send holds the mark before that delivery so the next sweep retries it', async () => {
  const gh = await fakeGithub([[d(1, 'a', 5, 503), d(2, 'b', 3, 200)]], true);
  const store = memory();
  await assert.rejects(redeliverFailed(gh.app, store, opts));
  assert.equal(store.since, NOW - 5 * 60_000 - 1);
  gh.close();
});

test('retries stop at the cap, counting attempts GitHub still lists', () => {
  const tries = [d(1, 'a', 9, 503), d(2, 'a', 8, 503, true), d(3, 'a', 7, 503, true), d(4, 'a', 6, 503, true)];
  assert.deepEqual(failedDeliveries(tries), []);
  assert.equal(failedDeliveries(tries.slice(0, 3)).length, 1);
});

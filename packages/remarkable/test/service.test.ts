import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { after, test } from 'node:test';
import { serveApp } from '@quack/sdk';
import { createRemarkableService, createRmClient } from '../src/index.ts';

const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

async function listen(server: Server) {
  servers.push(server.listen(0));
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

const EMAIL = 'me@example.com';
const PASSWORD = 'hunter2-secret';

// A fake rmfakecloud: plain-text JWT from login, a Bearer-guarded tree and PDF export. Each login mints a new token, and `expire` revokes the current one.
let token = '';
let logins = 0;
let notes = { version: 3, lastModified: '2026-10-01T10:00:00Z' };
const rm = await listen(
  createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    if (req.method === 'POST' && req.url === '/ui/api/login') {
      const b = JSON.parse(raw);
      if (b.email !== EMAIL || b.password !== PASSWORD) return res.writeHead(401).end('bad credentials');
      token = `jwt-${++logins}`;
      return res.writeHead(200).end(`${token}\n`);
    }
    if (req.headers.authorization !== `Bearer ${token}` || !token) return res.writeHead(401).end();
    if (req.url === '/ui/api/documents') {
      return res.writeHead(200, { 'content-type': 'application/json' }).end(
        JSON.stringify({
          Entries: [
            { id: 'n1', name: 'Notes', ...notes },
            { id: 'f1', name: 'Work', isFolder: true, lastModified: '2026-09-01T00:00:00Z', children: [{ id: 'n2', name: 'Plan', version: 1, lastModified: '2026-09-02T00:00:00Z' }] },
          ],
        }),
      );
    }
    if (req.url === '/ui/api/documents/n1?type=pdf') return res.writeHead(200).end('%PDF-fake');
    if (req.url === '/ui/api/documents/n1?type=rmdoc') return res.writeHead(200).end('PK-fake');
    if (req.url === '/ui/api/documents/html?type=rmdoc') return res.writeHead(200).end('<html>oops</html>');
    if (req.url === '/ui/api/documents/broken?type=pdf') return res.writeHead(500).end('json: cannot unmarshal object into Content.pageTags.0 of type string');
    if (req.url === '/ui/api/documents/html?type=pdf') return res.writeHead(200, { 'content-type': 'text/html' }).end('<html>oops</html>');
    if (req.url === '/ui/api/documents/empty?type=pdf') return res.writeHead(200, { 'content-type': 'application/pdf' }).end();
    res.writeHead(404).end();
  }),
);

test('client logs in, flattens and nests the tree, re-logs in on a 401, and downloads', async () => {
  const client = createRmClient({ url: `${rm}/`, email: EMAIL, password: PASSWORD });
  const before = logins;
  const tree = await client.tree();
  assert.equal(logins, before + 1, 'first call logs in after the empty token is refused');
  assert.deepEqual(tree.map((e) => [e.id, e.folder, e.version]), [['n1', false, '3'], ['f1', true, '2026-09-01T00:00:00Z']]);
  assert.deepEqual(tree[1].children?.map((c) => c.id), ['n2']);
  assert.deepEqual((await client.documents()).map((d) => d.id), ['n1', 'n2']);

  token = 'revoked';
  const pdf = await client.downloadPdf('n1');
  assert.equal(pdf.toString(), '%PDF-fake');
  assert.equal(logins, before + 2);
});

test('the raw document download is a zip, and an error page in its place is refused', async () => {
  const client = createRmClient({ url: rm, email: EMAIL, password: PASSWORD });
  assert.equal((await client.downloadRmdoc('n1')).toString(), 'PK-fake');
  await assert.rejects(client.downloadRmdoc('html'), /response is not a zip/);
});

test('a failed login names the status and leaks neither credential', async () => {
  const client = createRmClient({ url: rm, email: EMAIL, password: 'wrong-password' });
  await assert.rejects(client.tree(), (err: Error) => /status 401/.test(err.message) && !err.message.includes('wrong-password') && !err.message.includes(EMAIL));
});

// A fake distributor that answers a wait:true invocation only when the test releases that session.
type Seen = { key: string; agent: string | null; body: { input: string; requestId: string; title: string; wait: boolean } };
const seen: Seen[] = [];
const gates = new Map<string, (status: number) => void>();
let autoAnswer: number | undefined = 200;
let answerBody: unknown = { status: 'done' };
const arrived = new Set<() => void>();
const settledDocs: Array<() => void> = [];
const until = (ready: () => boolean) => new Promise<void>((resolve) => {
  const check = () => (ready() ? (arrived.delete(check), resolve()) : undefined);
  arrived.add(check);
  check();
});
const distributor = await listen(
  createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    const url = new URL(req.url!, 'http://x');
    const key = decodeURIComponent(url.pathname.split('/')[2]);
    seen.push({ key, agent: url.searchParams.get('agent'), body: JSON.parse(raw) });
    const answer = autoAnswer === undefined ? new Promise<number>((release) => gates.set(key, release)) : autoAnswer;
    arrived.forEach((f) => f());
    const status = await answer;
    res.writeHead(status).end(JSON.stringify(answerBody));
  }),
);

const service = createRemarkableService({
  dbPath: ':memory:',
  distributorUrl: distributor,
  agent: 'remarkable-ingest',
  client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }),
  onSettle: () => settledDocs.shift()?.(),
});
const base = await listen(serveApp(service));
const call = (method: string, path: string, body?: unknown, auth = 'Bearer svc-secret') =>
  fetch(base + path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
// Each notebook as the page sees it, with its ingest state; a settled ingest shows here.
const docs = async (url = base) => (((await (await fetch(`${url}/`)).json()) as any).tree as any[]).flatMap((e) => (e.folder ? e.children : [e]));
const doc = async (id: string, url = base) => (await docs(url)).find((d) => d.id === id);

test('picker data carries id, name, version, modified and state', async () => {
  const { tree } = (await (await call('GET', '/')).json()) as any;
  assert.deepEqual(tree[0], { id: 'n1', name: 'Notes', version: '3', modified: '2026-10-01T10:00:00Z', folder: false, state: 'new' });
  assert.equal(tree[1].children[0].name, 'Plan');
});

test('ingest submits one request per document at remarkable:<id>, and an unchanged document reuses its request id', async () => {
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1', 'n2', 'n1'] });
  await service.settled();
  assert.deepEqual(seen.map((s) => [s.key, s.agent, s.body.requestId, s.body.wait]), [['remarkable:n1', 'remarkable-ingest', '3', true], ['remarkable:n2', 'remarkable-ingest', '1', true]]);
  assert.ok(seen.every((s) => s.body.title && s.body.title === JSON.parse(s.body.input).name), 'the session title is the document name');
  assert.deepEqual((await docs()).map((d) => d.state), ['current', 'current']);

  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.equal(seen[0].body.requestId, '3', 'same version, same request id');

  notes = { version: 4, lastModified: '2026-10-02T10:00:00Z' };
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.equal(seen[0].body.requestId, '4', 'an edited document is a new request');
});

test('ingest rejects empty, malformed and unknown selections', async () => {
  assert.equal((await call('POST', '/ingest', { ids: [] })).status, 400);
  assert.equal((await call('POST', '/ingest', { ids: [7] })).status, 400);
  const unknown = await call('POST', '/ingest', { ids: ['n1', 'ghost'] });
  assert.equal(unknown.status, 404);
  assert.deepEqual((await problem(unknown, 404)).ids, ['ghost']);
  await problem(await call('POST', '/ingest', { ids: [] }), 400);
  await problem(await fetch(`${base}/ingest`, { method: 'POST', body: 'not json' }), 400);
});

test('an unknown id queues nothing, and a closed body refuses unknown fields', async () => {
  seen.length = 0;
  const before = await docs();
  await call('POST', '/ingest', { ids: ['n1', 'ghost'] });
  await problem(await call('POST', '/ingest', { ids: ['n1'], extra: 1 }), 400);
  await problem(await call('POST', '/ingest', { ids: ['n1'], requestId: '' }), 400);
  await service.settled();
  assert.deepEqual(seen, []);
  assert.deepEqual(await docs(), before);
});

test('an oversized body is a 413 problem', async () => {
  await problem(await call('POST', '/topics', { name: 'x'.repeat(2 << 20) }), 413);
});

test('a document is current only after its session has answered', async () => {
  autoAnswer = undefined;
  gates.clear();
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1', 'n2'] });
  // Wait for both submissions to reach the distributor, then answer one.
  await until(() => gates.size === 2);
  const first = new Promise<void>((r) => settledDocs.push(r));
  gates.get('remarkable:n1')!(200);
  await first;
  assert.deepEqual((await docs()).map((d) => d.state), ['current', 'ingesting']);

  gates.get('remarkable:n2')!(200);
  await service.settled();
  assert.equal((await doc('n2')).state, 'current');
});

test('a failed session settles the document failed with the reason', async () => {
  autoAnswer = 500;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  const done = await doc('n1');
  assert.equal(done.state, 'failed');
  assert.equal(done.error, 'distributor answered 500');
  autoAnswer = 200;
});

test('a restart resubmits pending items with the same request id and the document completes', async () => {
  const dir = mkdtempSync(`${tmpdir()}/rm-`);
  after(() => rmSync(dir, { recursive: true, force: true }));
  const requests: string[] = [];
  const held: Array<(status: number) => void> = [];
  const waiters = new Set<() => void>();
  const slow = await listen(
    createServer(async (req, res) => {
      let raw = '';
      for await (const c of req) raw += c;
      requests.push(JSON.parse(raw).requestId);
      const answer = new Promise<number>((release) => held.push(release));
      waiters.forEach((w) => w());
      res.writeHead(await answer).end('{}');
    }),
  );
  const arrivedAtLeast = (n: number) => new Promise<void>((resolve) => {
    const w = () => (requests.length >= n ? (waiters.delete(w), resolve()) : undefined);
    waiters.add(w);
    w();
  });
  const start = () => createRemarkableService({ dbPath: `${dir}/rm.db`, distributorUrl: slow, agent: 'a', client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }) });
  const headers = { authorization: 'Bearer svc-secret' };

  const first = start();
  const firstUrl = await listen(serveApp(first));
  await fetch(`${firstUrl}/ingest`, { method: 'POST', headers, body: JSON.stringify({ ids: ['n2'] }) });
  await arrivedAtLeast(1);
  await first.stop();

  const second = start();
  const secondUrl = await listen(serveApp(second));
  await arrivedAtLeast(2);
  assert.deepEqual(requests, ['1', '1'], 'resubmitted with the stored request id');
  held[1](200);
  await second.settled();
  assert.equal((await doc('n2', secondUrl)).state, 'current');
  await second.stop();
});

test('ingest runs at most `concurrency` documents at once, oldest first, and the queue survives a restart', async () => {
  const dir = mkdtempSync(`${tmpdir()}/rm-`);
  after(() => rmSync(dir, { recursive: true, force: true }));
  // The distributor as an injected fetch: a submission is recorded the moment it is made and held until released.
  const keys: string[] = [];
  const held: Array<() => void> = [];
  let onCall = () => {};
  const distributorFetch = (async (url: string, init: RequestInit) => {
    keys.push(decodeURIComponent(new URL(url).pathname.split('/')[2]));
    onCall();
    return new Promise<Response>((resolve, reject) => {
      held.push(() => resolve(new Response('{"status":"done"}')));
      init.signal?.addEventListener('abort', () => reject(init.signal!.reason));
    });
  }) as typeof fetch;
  const start = () => createRemarkableService({ dbPath: `${dir}/rm.db`, distributorUrl: 'http://distributor.test', fetch: distributorFetch, agent: 'a', concurrency: 1, client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }) });
  const headers = { authorization: 'Bearer svc-secret' };

  const first = start();
  const firstUrl = await listen(serveApp(first));
  await fetch(`${firstUrl}/ingest`, { method: 'POST', headers, body: JSON.stringify({ ids: ['n2', 'n1'] }) });
  assert.deepEqual(keys, ['remarkable:n2'], 'the second document waits for the first');
  await first.stop();

  const second = start();
  const secondUrl = await listen(serveApp(second));
  assert.deepEqual(keys, ['remarkable:n2', 'remarkable:n2'], 'after a restart the head of the queue goes first, alone');
  const next = new Promise<void>((resolve) => (onCall = resolve));
  held[1]();
  await next;
  assert.equal(keys[2], 'remarkable:n1', 'the next queued document starts once the first is answered');
  held[2]();
  await second.settled();
  assert.deepEqual((await docs(secondUrl)).map((d) => d.state), ['current', 'current']);
  await second.stop();
});

test('a 200 whose result is unanswered settles the item failed with the reason', async () => {
  answerBody = { status: 'unanswered', reason: 'aborted by operator' };
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.deepEqual(await doc('n1').then((d) => [d.state, d.error]), ['failed', 'session unanswered: aborted by operator']);
  answerBody = { status: 'done' };
});

test('an unreachable distributor settles the item failed', async () => {
  const down = createRemarkableService({ dbPath: ':memory:', distributorUrl: 'http://127.0.0.1:1', agent: 'remarkable-ingest', client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }) });
  const downBase = await listen(serveApp(down));
  const headers = { authorization: 'Bearer svc-secret' };
  await fetch(`${downBase}/ingest`, { method: 'POST', headers, body: JSON.stringify({ ids: ['n1'] }) });
  await down.settled();
  const got = await doc('n1', downBase);
  assert.equal(got.state, 'failed');
  assert.match(got.error, /distributor unreachable/);
});

test('an unanswered session records the workflow failure detail on the document', async () => {
  answerBody = { status: 'unanswered', reason: 'failed', detail: 'rmfakecloud could not export Notes: status 500' };
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.equal((await doc('n1')).error, 'session unanswered: failed: rmfakecloud could not export Notes: status 500');
  answerBody = { status: 'done' };
});

test('the client puts the first line of the rmfakecloud error body in the reason', async () => {
  const client = createRmClient({ url: rm, email: EMAIL, password: PASSWORD });
  await assert.rejects(client.downloadPdf('broken'), /status 500: json: cannot unmarshal object into Content\.pageTags\.0 of type string$/);
});

// A service over a stub notebook, for the routes that need no distributor.
async function stub(client: Partial<Parameters<typeof createRemarkableService>[0]['client']> = {}) {
  const svc = createRemarkableService({ dbPath: ':memory:', distributorUrl: 'http://unused', agent: 'a', client: { tree: async () => [], documents: async () => [], downloadPdf: async () => Buffer.from('%PDF-'), downloadRmdoc: async () => Buffer.from('PK'), ...client } });
  return { svc, url: await listen(serveApp(svc)) };
}
const listed = (...ids: string[]) => async () => ids.map((id) => ({ id, name: id, version: '1', modified: '' }));
const problem = async (res: Response, status: number) => {
  assert.equal(res.status, status);
  assert.equal(res.headers.get('content-type'), 'application/problem+json');
  const p = (await res.json()) as any;
  assert.equal(p.status, status);
  assert.equal(p.error, p.detail);
  assert.ok(p.type && p.title && p.detail);
  return p;
};

test('a failed or empty export of a listed document is a 502 and the service keeps serving', async () => {
  const failing = async () => { throw new Error('rmfakecloud GET /x: response is not a PDF'); };
  const { url } = await stub({ documents: listed('d', 'ok'), downloadPdf: async (id) => (id === 'd' ? failing() : Buffer.from('%PDF-')), downloadRmdoc: failing });
  await problem(await fetch(`${url}/documents/d/pdf`), 502);
  await problem(await fetch(`${url}/documents/d/rmdoc`), 502);
  assert.equal((await fetch(`${url}/documents/ok/pdf`)).status, 200);
});

test('a document the notebook does not list is a 404 problem, not an upstream 502', async () => {
  const { url } = await stub({ documents: listed('d') });
  for (const kind of ['pdf', 'rmdoc']) await problem(await fetch(`${url}/documents/ghost/${kind}`), 404);
  assert.equal((await fetch(`${url}/documents/d/rmdoc`)).status, 200);
});

test('a malformed document id is a 400, not an upstream error', async () => {
  assert.equal((await call('GET', '/documents/%E0%A4%A/pdf')).status, 400);
});

test('a failed ingest of an unchanged document retries as a new attempt, and a re-ingest after it joins that attempt', async () => {
  notes = { version: 9, lastModified: '2026-10-09T10:00:00Z' };
  autoAnswer = 500;
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  autoAnswer = 200;
  for (let i = 0; i < 2; i++) {
    await call('POST', '/ingest', { ids: ['n1'] });
    await service.settled();
  }
  assert.deepEqual(seen.map((s) => s.body.requestId), ['9', '9#1', '9#1']);
});

test('the page routes list notebook states and ingest a failed one as a new attempt', async () => {
  notes = { version: 11, lastModified: '2026-10-11T10:00:00Z' };
  const states = async () => (await docs()).map((e) => [e.id, e.state]);
  assert.equal((await call('GET', '/documents/x/other')).status, 404);
  assert.equal((await call('GET', '/ingest')).status, 404);
  const before = await states();
  assert.equal(before.find((s) => s[0] === 'n1')![1], 'changed', 'edited since earlier tests ingested it');
  autoAnswer = 500;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.equal((await states()).find((s) => s[0] === 'n1')![1], 'failed');
  autoAnswer = 200;
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.deepEqual(seen.map((s) => s.body.requestId), ['11#1']);
  assert.equal((await states()).find((s) => s[0] === 'n1')![1], 'current');
  notes = { version: 12, lastModified: '2026-10-12T10:00:00Z' };
  assert.equal((await states()).find((s) => s[0] === 'n1')![1], 'changed');
});

test('a 502, a 504 or a reset mid-wait resubmits the same request id with backoff and the item still completes', async () => {
  const requests: string[] = [];
  const script = ['502', '504', 'reset', '200'];
  const flaky = await listen(
    createServer(async (req, res) => {
      let raw = '';
      for await (const c of req) raw += c;
      requests.push(JSON.parse(raw).requestId);
      const next = script[requests.length - 1];
      if (next === 'reset') return void req.socket.destroy();
      res.writeHead(Number(next)).end('{"status":"done"}');
    }),
  );
  const delays: number[] = [];
  const retrying = createRemarkableService({ dbPath: ':memory:', distributorUrl: flaky, agent: 'a', client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }), sleep: async (ms) => void delays.push(ms) });
  const url = await listen(serveApp(retrying));
  const headers = { authorization: 'Bearer svc-secret' };
  await fetch(`${url}/ingest`, { method: 'POST', headers, body: JSON.stringify({ ids: ['n1'] }) });
  await retrying.settled();
  assert.equal((await doc('n1', url)).state, 'current');
  assert.equal(new Set(requests).size, 1);
  assert.equal(requests.length, 4);
  assert.deepEqual(delays, [1000, 2000, 4000]);
  await retrying.stop();
});

test('a requestId returns the original batch without requeueing, across a restart', async () => {
  const dir = mkdtempSync(`${tmpdir()}/rm-`);
  after(() => rmSync(dir, { recursive: true, force: true }));
  const submitted: string[] = [];
  const fetchOk = (async (url: string) => (submitted.push(url), new Response('{"status":"done"}'))) as unknown as typeof fetch;
  const start = () => createRemarkableService({ dbPath: `${dir}/rm.db`, distributorUrl: 'http://d.test', fetch: fetchOk, agent: 'a', client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }) });
  const post = (url: string, body: object) => fetch(`${url}/ingest`, { method: 'POST', body: JSON.stringify(body) });
  const first = start();
  const firstUrl = await listen(serveApp(first));
  const a = (await (await post(firstUrl, { ids: ['n2'], requestId: 'k1' })).json()) as any;
  await first.settled();
  const again = (await (await post(firstUrl, { ids: ['n2'], requestId: 'k1' })).json()) as any;
  assert.equal(again.batch, a.batch);
  const other = (await (await post(firstUrl, { ids: ['n2'], requestId: 'k2' })).json()) as any;
  assert.notEqual(other.batch, a.batch);
  await first.stop();

  const second = start();
  const secondUrl = await listen(serveApp(second));
  const n = submitted.length;
  assert.equal(((await (await post(secondUrl, { ids: ['n2'], requestId: 'k1' })).json()) as any).batch, a.batch);
  assert.equal(submitted.length, n, 'the resubmission queued nothing');
  await second.stop();
});

test('two concurrent requests with one requestId share one batch', async () => {
  const fetchOk = (async () => new Response('{"status":"done"}')) as unknown as typeof fetch;
  // Both requests must be past the key lookup before either creates, so the listing holds until two have asked.
  let asked = 0;
  const bothAsked = Promise.withResolvers<void>();
  const client = { tree: async () => [], documents: async () => (++asked === 2 && bothAsked.resolve(), await bothAsked.promise, [{ id: 'n2', name: 'n2', version: '1', modified: '' }]), downloadPdf: async () => Buffer.alloc(0), downloadRmdoc: async () => Buffer.alloc(0) };
  const svc = createRemarkableService({ dbPath: ':memory:', distributorUrl: 'http://d.test', fetch: fetchOk, agent: 'a', client });
  const url = await listen(serveApp(svc));
  after(() => svc.stop());
  const post = () => fetch(`${url}/ingest`, { method: 'POST', body: JSON.stringify({ ids: ['n2'], requestId: 'race' }) });
  const answers = await Promise.all([post(), post()]);
  assert.deepEqual(answers.map((r) => r.status), [202, 202]);
  const [a, b] = (await Promise.all(answers.map((r) => r.json()))) as any[];
  assert.equal(a.batch, b.batch);
});

test('topics are paged by name with a token, and a bad pageSize or token is a 400', async () => {
  const { url } = await stub();
  const add = (name: string) => fetch(`${url}/topics`, { method: 'POST', body: JSON.stringify({ name }) });
  for (const n of ['pear', 'Apple', 'cherry', 'banana']) await add(n);
  const get = async (q = '') => (await (await fetch(`${url}/topics${q}`)).json()) as any;
  assert.deepEqual(await get(), { topics: ['Apple', 'banana', 'cherry', 'pear'] });
  const one = await get('?pageSize=3');
  assert.deepEqual(one.topics, ['Apple', 'banana', 'cherry']);
  const two = await get(`?pageSize=3&pageToken=${one.nextPageToken}`);
  assert.deepEqual(two, { topics: ['pear'] });
  for (const q of ['?pageSize=0', '?pageSize=1001', '?pageSize=x', '?pageSize=1.5', '?pageToken=junk']) await problem(await fetch(`${url}/topics${q}`), 400);
  assert.equal((await fetch(`${url}/topics?pageSize=1000`)).status, 200);
});

test('POST /topics is 201 for a new topic and 200 for a duplicate, trims the name and closes the body', async () => {
  const { url } = await stub();
  const add = (body: unknown) => fetch(`${url}/topics`, { method: 'POST', body: JSON.stringify(body) });
  const made = await add({ name: '  Home   renovation ' });
  assert.deepEqual([made.status, await made.json()], [201, { name: 'Home renovation' }]);
  const dup = await add({ name: 'home RENOVATION' });
  assert.deepEqual([dup.status, await dup.json()], [200, { name: 'Home renovation' }]);
  assert.equal((await add({ name: 'x'.repeat(40) })).status, 201);
  for (const body of [{ name: 'x'.repeat(41) }, { name: '   ' }, {}, { name: 'a', extra: 1 }, [], null]) await problem(await add(body), 400);
});

test('other methods on /topics are a 405 with Allow: GET, POST', async () => {
  const { url } = await stub();
  for (const method of ['PUT', 'PATCH', 'DELETE']) {
    const res = await fetch(`${url}/topics`, { method });
    assert.equal(res.headers.get('allow'), 'GET, POST');
    await problem(res, 405);
  }
});

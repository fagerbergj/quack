import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { after, test } from 'node:test';
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
type Seen = { key: string; agent: string | null; body: { input: string; requestId: string; wait: boolean } };
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
  serviceToken: 'svc-secret',
  dbPath: ':memory:',
  distributorUrl: distributor,
  agent: 'remarkable-ingest',
  client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }),
  onSettle: () => settledDocs.shift()?.(),
});
const base = await listen(service);
const call = (method: string, path: string, body?: unknown, auth = 'Bearer svc-secret') =>
  fetch(base + path, { method, headers: { authorization: auth }, body: body === undefined ? undefined : JSON.stringify(body) });
const batch = async (id: string) => (await call('GET', `/batches/${id}`)).json() as Promise<any>;

test('picker data needs the service token and carries id, name, version and modified', async () => {
  assert.equal((await call('GET', '/documents', undefined, 'Bearer nope')).status, 401);
  const { documents } = (await (await call('GET', '/documents')).json()) as any;
  assert.deepEqual(documents[0], { id: 'n1', name: 'Notes', version: '3', modified: '2026-10-01T10:00:00Z', folder: false });
  assert.equal(documents[1].children[0].name, 'Plan');
});

test('ingest submits one request per document at remarkable:<id>, and an unchanged document reuses its request id', async () => {
  seen.length = 0;
  const first = (await (await call('POST', '/ingest', { ids: ['n1', 'n2', 'n1'] })).json()) as any;
  await service.settled();
  assert.deepEqual(seen.map((s) => [s.key, s.agent, s.body.requestId, s.body.wait]), [['remarkable:n1', 'remarkable-ingest', '3', true], ['remarkable:n2', 'remarkable-ingest', '1', true]]);
  assert.equal((await batch(first.batch)).status, 'done');

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
  assert.deepEqual(((await unknown.json()) as any).ids, ['ghost']);
  assert.equal((await call('GET', '/batches/nope')).status, 404);
});

test('a batch is done only after every document session has answered', async () => {
  autoAnswer = undefined;
  gates.clear();
  seen.length = 0;
  const { batch: id } = (await (await call('POST', '/ingest', { ids: ['n1', 'n2'] })).json()) as any;
  // Wait for both submissions to reach the distributor, then answer one.
  await until(() => gates.size === 2);
  const first = new Promise<void>((r) => settledDocs.push(r));
  gates.get('remarkable:n1')!(200);
  await first;
  const partial = await batch(id);
  assert.equal(partial.status, 'pending');
  assert.deepEqual(partial.items.map((i: any) => i.status), ['done', 'pending']);

  gates.get('remarkable:n2')!(200);
  await service.settled();
  assert.equal((await batch(id)).status, 'done');
});

test('a batch with a failed session settles as failed with the reason', async () => {
  autoAnswer = 500;
  const { batch: id } = (await (await call('POST', '/ingest', { ids: ['n1'] })).json()) as any;
  await service.settled();
  const done = await batch(id);
  assert.equal(done.status, 'failed');
  assert.equal(done.items[0].error, 'distributor answered 500');
  autoAnswer = 200;
});

test('a restart resubmits pending items with the same request id and the batch completes', async () => {
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
  const start = () => createRemarkableService({ serviceToken: 'svc-secret', dbPath: `${dir}/rm.db`, distributorUrl: slow, agent: 'a', client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }) });
  const headers = { authorization: 'Bearer svc-secret' };

  const first = start();
  const firstUrl = await listen(first);
  const { batch: id } = (await (await fetch(`${firstUrl}/ingest`, { method: 'POST', headers, body: JSON.stringify({ ids: ['n2'] }) })).json()) as any;
  await arrivedAtLeast(1);
  await first.stop();

  const second = start();
  const secondUrl = await listen(second);
  await arrivedAtLeast(2);
  assert.deepEqual(requests, ['1', '1'], 'resubmitted with the stored request id');
  held[1](200);
  await second.settled();
  const done = (await (await fetch(`${secondUrl}/batches/${id}`, { headers })).json()) as any;
  assert.equal(done.status, 'done');
  await second.stop();
});

test('a 200 whose result is unanswered settles the item failed with the reason', async () => {
  answerBody = { status: 'unanswered', reason: 'aborted by operator' };
  const { batch: id } = (await (await call('POST', '/ingest', { ids: ['n1'] })).json()) as any;
  await service.settled();
  const done = await batch(id);
  assert.equal(done.status, 'failed');
  assert.equal(done.items[0].error, 'session unanswered: aborted by operator');
  answerBody = { status: 'done' };
});

test('an unreachable distributor settles the item failed', async () => {
  const down = createRemarkableService({ serviceToken: 'svc-secret', dbPath: ':memory:', distributorUrl: 'http://127.0.0.1:1', agent: 'remarkable-ingest', client: createRmClient({ url: rm, email: EMAIL, password: PASSWORD }) });
  const downBase = await listen(down);
  const headers = { authorization: 'Bearer svc-secret' };
  const { batch: id } = (await (await fetch(`${downBase}/ingest`, { method: 'POST', headers, body: JSON.stringify({ ids: ['n1'] }) })).json()) as any;
  await down.settled();
  const got = (await (await fetch(`${downBase}/batches/${id}`, { headers })).json()) as any;
  assert.equal(got.status, 'failed');
  assert.match(got.items[0].error, /distributor unreachable/);
});

test('an unanswered session records the workflow failure detail on the batch item', async () => {
  answerBody = { status: 'unanswered', reason: 'failed', detail: 'rmfakecloud could not export Notes: status 500' };
  const { batch: id } = (await (await call('POST', '/ingest', { ids: ['n1'] })).json()) as any;
  await service.settled();
  assert.equal((await batch(id)).items[0].error, 'session unanswered: failed: rmfakecloud could not export Notes: status 500');
  answerBody = { status: 'done' };
});

test('the client puts the first line of the rmfakecloud error body in the reason', async () => {
  const client = createRmClient({ url: rm, email: EMAIL, password: PASSWORD });
  await assert.rejects(client.downloadPdf('broken'), /status 500: json: cannot unmarshal object into Content\.pageTags\.0 of type string$/);
});

test('a failed or empty PDF export is a 502 and the service keeps serving', async () => {
  for (const id of ['broken', 'empty', 'html']) assert.equal((await call('GET', `/documents/${id}/pdf`)).status, 502, id);
  assert.equal((await call('GET', '/documents/n1/pdf')).status, 200);
  assert.equal((await call('GET', '/documents/n1/rmdoc')).status, 200);
  assert.equal((await call('GET', '/documents/html/rmdoc')).status, 502);
});

test('a malformed batch id is a 400, not an upstream error', async () => {
  assert.equal((await call('GET', '/batches/%E0%A4%A')).status, 400);
});

test('a failed ingest of an unchanged document retries as a new attempt; a retry of a succeeded or untried item is refused', async () => {
  notes = { version: 9, lastModified: '2026-10-09T10:00:00Z' };
  assert.equal((await call('POST', '/ingest', { ids: ['n1'], retry: true })).status, 409, 'nothing has been tried yet');
  autoAnswer = 500;
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  autoAnswer = 200;
  await call('POST', '/ingest', { ids: ['n1'], retry: true });
  await service.settled();
  assert.deepEqual(seen.map((s) => s.body.requestId), ['9', '9#1']);
  const refused = await call('POST', '/ingest', { ids: ['n1'], retry: true });
  assert.equal(refused.status, 409);
  assert.deepEqual(((await refused.json()) as any).ids, ['n1']);
  seen.length = 0;
  await call('POST', '/ingest', { ids: ['n1'] });
  await service.settled();
  assert.equal(seen[0].body.requestId, '9#1', 'a plain re-ingest joins the latest attempt');
});

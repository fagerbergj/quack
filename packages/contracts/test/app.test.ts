import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createCatalog, fileJournal, sessionsApi, sqliteCatalog, type CatalogEvent } from '@quack/catalog';
import { inProcessFetch, serveApp } from '@quack/sdk';
import { manifest } from '../src/manifest.ts';

const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

let seq = 0;
const ev = (type: string, data: unknown, id = `${type}:${seq + 1}`) => ({ seq: ++seq, id, at: seq * 1000, type, data }) as CatalogEvent;
const spec = (version: number, title = 'Sleeper schedule endpoint', kind = 'rest') => ev('artifact.sealed', { kind: 'contract.spec', version: version + 10, hash: `h${version}`, content: JSON.stringify({ kind, title, version, intent: {}, document: {} }) });
const record = (kind: string, n: number, version: number) => ev('artifact.sealed', { kind, version: n, hash: `r${n}`, content: JSON.stringify({ version, note: '', approval: `a${n}` }) });
const proposal = (id: string, version: number) => ev('ask.opened', { id, type: 'approval', createdAt: seq, toolCall: { tool: 'propose_contract', args: { version } } });

async function setup(wrap: (f: typeof fetch) => typeof fetch = (f) => f) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-contracts-app-'));
  const store = sqliteCatalog(join(dir, 'c.sqlite'), { currency: 'USD' });
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  cleanups.push(async () => { await store.close(); await rm(dir, { recursive: true, force: true }); });
  // The distributor as the app reaches it in process: the Sessions API over the catalog, called as the tailnet.
  const api = sessionsApi(catalog, { deleteSession: async () => 'deleted', answer: async () => true });
  const fetch = wrap(inProcessFetch(async (req, res) => void ((await api(req, res, { kind: 'tailnet' })) || res.writeHead(404).end('{}'))));
  const app = await manifest.app!({ settings: {}, dbPath: ':memory:', distributorUrl: 'http://distributor.local', fetch, peerFetch: () => fetch, deployment: {}, env: {} });
  const server = serveApp(app);
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  cleanups.push(() => new Promise((r) => server.close(r)));
  return { catalog, url: `http://127.0.0.1:${(server.address() as { port: number }).port}` };
}

const created = (key: string) => ev('session.created', { agent: 'contracts', createdAt: 0 }, `session:${key}`);

test("GET /contracts reads each contract's title, kind and versions from its sealed artifacts, and whether an approval waits", async () => {
  const { catalog, url } = await setup();
  seq = 0;
  await catalog.publish('contracts:a', [created('contracts:a'), spec(1), spec(2), proposal('p1', 1), record('contract.approved', 1, 1), proposal('p2', 2)]);
  seq = 0;
  await catalog.publish('contracts:b', [created('contracts:b'), ev('title.set', { title: 'draft title' }), spec(3, 'Extension SDK', 'typescript'), proposal('p3', 3), record('contract.changes', 1, 3)]);
  await catalog.resolve('contracts:a', 'p1', { by: { kind: 'tailnet' }, choice: 'approve' });
  await catalog.resolve('contracts:b', 'p3', { by: { kind: 'tailnet' }, choice: 'reject' });
  seq = 0;
  await catalog.publish('contracts:gone', [created('contracts:gone'), spec(1)]);
  await catalog.archive('contracts:gone');
  seq = 0;
  await catalog.publish('review:x', [created('review:x')]);
  const res = await fetch(`${url}/contracts`);
  assert.equal(res.status, 200);
  const { contracts } = (await res.json()) as { contracts: Array<Record<string, unknown>> };
  assert.deepEqual(contracts.sort((x, y) => String(x.key).localeCompare(String(y.key))), [
    { key: 'contracts:a', title: 'Sleeper schedule endpoint', kind: 'rest', draft: 2, proposed: 2, approved: 1, pendingApproval: true },
    { key: 'contracts:b', title: 'Extension SDK', kind: 'typescript', draft: 3, proposed: 3, changesRequested: 3, pendingApproval: false },
  ]);
});

test('a contract session with nothing sealed yet reads as its session title, and any other route is not found', async () => {
  const { catalog, url } = await setup();
  seq = 0;
  await catalog.publish('contracts:new', [created('contracts:new'), ev('title.set', { title: 'Payments API' })]);
  const { contracts } = (await (await fetch(`${url}/contracts`)).json()) as { contracts: unknown[] };
  assert.deepEqual(contracts, [{ key: 'contracts:new', title: 'Payments API', kind: 'rest', pendingApproval: false }]);
  assert.equal((await fetch(`${url}/nope`)).status, 404);
  const page = manifest.pages![0]!.path as RegExp;
  assert.deepEqual(['/contracts', '/contracts/contracts%3Aa', '/contracts/a/b', '/other'].map((p) => page.test(p)), [true, true, false, false], 'the page reaches both reads through the proxy');
});

test('GET /contracts/{key} gives each sealed spec its status from the approval records and proposals, never from a verdict', async () => {
  const { catalog, url } = await setup();
  seq = 0;
  const offer = (id: string, version: number, artifact: number) => ev('ask.opened', { id, type: 'approval', createdAt: seq, toolCall: { tool: 'propose_contract', args: { version, artifact, hash: `h${version}` } } });
  const decision = (kind: string, n: number, version: number, artifact: number) => ev('artifact.sealed', { kind, version: n, hash: `d${n}`, content: JSON.stringify({ version, artifact, note: '', approval: `p${n}` }) });
  await catalog.publish('contracts:s', [
    created('contracts:s'), spec(1), offer('p1', 1, 11), decision('contract.approved', 1, 1, 11),
    spec(2), offer('p2', 2, 12), decision('contract.changes', 1, 2, 12),
    spec(3), offer('p3', 3, 13), spec(4), offer('p4', 4, 14),
    ev('artifact.sealed', { kind: 'contract.spec', version: 15, hash: 'h4b', content: JSON.stringify({ kind: 'rest', title: 'Sleeper schedule endpoint', version: 4, intent: {}, document: {} }) }),
  ]);
  await catalog.resolve('contracts:s', 'p1', { by: { kind: 'tailnet' }, choice: 'approve' });
  await catalog.resolve('contracts:s', 'p2', { by: { kind: 'tailnet' }, choice: 'reject' });
  // Approved, its record not sealed yet: the answered proposal says so.
  await catalog.resolve('contracts:s', 'p3', { by: { kind: 'tailnet' }, choice: 'approve' });
  const res = await fetch(`${url}/contracts/${encodeURIComponent('contracts:s')}`);
  assert.equal(res.status, 200);
  const body = (await res.json()) as { key: string; pendingApproval: boolean; versions: unknown[] };
  assert.deepEqual([body.key, body.pendingApproval], ['contracts:s', true]);
  assert.deepEqual(body.versions, [
    { artifact: 11, version: 1, status: 'approved' },
    { artifact: 12, version: 2, status: 'changes_requested' },
    { artifact: 13, version: 3, status: 'approved' },
    { artifact: 14, version: 4, status: 'proposed' },
    { artifact: 15, version: 4, status: 'draft' },
  ]);
  for (const key of ['contracts:missing', 'review:x', '%E0']) assert.equal((await fetch(`${url}/contracts/${encodeURIComponent(key)}`)).status, 404, key);
});

// Five contracts: a and c wait on an approval; d has sealed nothing; archived and review sessions never list.
async function five(catalog: Awaited<ReturnType<typeof setup>>['catalog']) {
  const seeds: Record<string, () => CatalogEvent[]> = {
    a: () => [spec(1), proposal('p1', 1)],
    b: () => [spec(3, 'Extension SDK', 'typescript')],
    c: () => [spec(2, 'Payments API', 'json-schema'), proposal('p2', 2)],
    d: () => [ev('title.set', { title: 'Draft d' })],
    e: () => [spec(1, 'Sleeper roster')],
  };
  for (const [id, events] of Object.entries(seeds)) {
    seq = 0;
    await catalog.publish(`contracts:${id}`, [created(`contracts:${id}`), ...events()]);
  }
  seq = 0;
  await catalog.publish('contracts:gone', [created('contracts:gone'), spec(1)]);
  await catalog.archive('contracts:gone');
  seq = 0;
  await catalog.publish('review:x', [created('review:x')]);
}

type Page = { contracts: Array<{ key: string }>; nextPageToken?: string };
// Every page of one listing, as the keys each held.
async function pages(url: string, query: string): Promise<string[][]> {
  const out: string[][] = [];
  for (let token: string | undefined = ''; token !== undefined;) {
    const res: Response = await fetch(`${url}/contracts?${query}${token && `&pageToken=${encodeURIComponent(token)}`}`);
    assert.equal(res.status, 200, await res.clone().text());
    const page = (await res.json()) as Page;
    out.push(page.contracts.map((c) => c.key));
    token = page.nextPageToken;
  }
  return out;
}
const keys = (ids: string) => ids.split('').map((id) => `contracts:${id}`);
const spy = (calls: string[]) => (f: typeof fetch): typeof fetch => (input, init) => (calls.push(String(input)), f(input, init));

test('GET /contracts pages by pageSize with no record skipped or repeated, and reads only the sessions of the page it returns', async () => {
  const calls: string[] = [];
  const { catalog, url } = await setup(spy(calls));
  await five(catalog);
  calls.length = 0;
  const all = await pages(url, 'pageSize=2');
  assert.deepEqual(all.map((p) => p.length), [2, 2, 1]);
  assert.deepEqual(all.flat().sort(), keys('abcde'));
  assert.equal(calls.filter((c) => /\/sessions\/contracts%3A/.test(c)).length, 2 * 5, 'two reads per listed session, each read once');
  calls.length = 0;
  await fetch(`${url}/contracts`);
  assert.match(calls[0]!, /[?&]pageSize=100(&|$)/, 'the default page is 100');
});

test('GET /contracts filters by q on key, title, kind and pendingApproval, ANDing terms and negating with -', async () => {
  const { catalog, url } = await setup();
  await five(catalog);
  const listed = async (q: string) => (await pages(url, `q=${encodeURIComponent(q)}`)).flat().sort();
  assert.deepEqual(await listed('key:contracts\\:a'), keys('a'));
  assert.deepEqual(await listed('key:contracts\\:? -key:*b'), keys('acde'), 'a ? wildcard and a negated key');
  assert.deepEqual(await listed('title:"Extension SDK"'), keys('b'), 'the spec title, not the session title');
  assert.deepEqual(await listed('title:Sleeper*'), keys('ae'));
  assert.deepEqual(await listed('title:Draft?d'), keys('d'), 'the session title when nothing is sealed');
  assert.deepEqual(await listed('kind:typescript'), keys('b'));
  assert.deepEqual(await listed('kind:rest'), keys('ade'), 'rest when no spec names a kind');
  assert.deepEqual(await listed('pendingApproval:true'), keys('ac'));
  assert.deepEqual(await listed('-pendingApproval:true'), keys('bde'));
  assert.deepEqual(await listed('pendingApproval:true kind:rest'), keys('a'));
  assert.deepEqual(await listed('kind:typescript OR (title:Payments* AND NOT key:contracts\\:z)'), keys('bc'));
});

test('GET /contracts pushes key terms down to the catalog, and fills a filtered page across catalog pages', async () => {
  const calls: string[] = [];
  const { catalog, url } = await setup(spy(calls));
  await five(catalog);
  calls.length = 0;
  assert.deepEqual(await pages(url, `q=${encodeURIComponent('-key:contracts\\:b pendingApproval:false')}`), [keys('de')]);
  assert.equal(new URL(calls[0]!).searchParams.get('q'), 'key:contracts\\:* AND -state:archived AND (-key:contracts\\:b)');
  const paged = await pages(url, `pageSize=1&q=${encodeURIComponent('pendingApproval:false')}`);
  assert.deepEqual(paged.flat().sort(), keys('bde'));
  assert.ok(paged.slice(0, -1).every((p) => p.length === 1), 'each page but the last is full');
});

test('GET /contracts answers 400 problem+json for a bad pageSize, an unreadable pageToken, or a q it cannot run', async () => {
  const { catalog, url } = await setup();
  await five(catalog);
  const bad = ['pageSize=0', 'pageSize=1001', 'pageSize=two', 'pageSize=', 'pageToken=nonsense',
    ...['nope:x', 'kind:soap', 'kind:re*', 'pendingApproval:maybe', 'Sleeper', 'title:[a TO b]', 'key:>a', 'title:/x/', '(key:a'].map((q) => `q=${encodeURIComponent(q)}`)];
  for (const query of bad) {
    const res = await fetch(`${url}/contracts?${query}`);
    assert.equal(res.status, 400, query);
    assert.equal(res.headers.get('content-type'), 'application/problem+json', query);
    const body = (await res.json()) as Record<string, unknown>;
    assert.deepEqual([body.type, body.title, body.status, body.error], ['about:blank', 'Bad Request', 400, body.detail], query);
    assert.ok(typeof body.detail === 'string' && body.detail, query);
  }
  const { nextPageToken } = (await (await fetch(`${url}/contracts?pageSize=1`)).json()) as Page;
  const other = await fetch(`${url}/contracts?pageSize=1&pageToken=${encodeURIComponent(nextPageToken!)}&q=${encodeURIComponent('key:contracts\\:a*')}`);
  assert.equal(other.status, 400, 'a token issued for another q');
});

test('a pageToken is bound to the whole q: changing any term, a post-read one included, answers 400', async () => {
  const { catalog, url } = await setup();
  await five(catalog);
  const first = (await (await fetch(`${url}/contracts?pageSize=1&q=${encodeURIComponent('kind:rest')}`)).json()) as Page;
  assert.ok(first.nextPageToken);
  const token = encodeURIComponent(first.nextPageToken);
  const res = await fetch(`${url}/contracts?pageSize=1&q=${encodeURIComponent('kind:typescript')}&pageToken=${token}`);
  assert.equal(res.status, 400);
  assert.match(((await res.json()) as { detail: string }).detail, /different q/);
  assert.equal((await fetch(`${url}/contracts?pageSize=1&q=${encodeURIComponent('kind:rest')}&pageToken=${token}`)).status, 200, 'the same q pages on');
});

test('a filter that matches nothing walks a bounded number of catalog pages, then answers a short page with a token', async () => {
  const calls: string[] = [];
  const { catalog, url } = await setup(spy(calls));
  await five(catalog);
  for (let i = 0; i < 12; i++) {
    seq = 0;
    await catalog.publish(`contracts:z${i}`, [created(`contracts:z${i}`)]);
  }
  calls.length = 0;
  const q = encodeURIComponent('title:nothing');
  const first = (await (await fetch(`${url}/contracts?pageSize=1&q=${q}`)).json()) as Page;
  assert.deepEqual(first.contracts, []);
  assert.ok(first.nextPageToken, 'a token while catalog rows remain');
  assert.equal(calls.filter((c) => c.includes('/sessions?')).length, 10, 'ten catalog pages at most');
  const second = (await (await fetch(`${url}/contracts?pageSize=1&q=${q}&pageToken=${encodeURIComponent(first.nextPageToken)}`)).json()) as Page;
  assert.deepEqual([second.contracts, second.nextPageToken], [[], undefined], 'the remaining 7 sessions end the list');
});

test('GET /contracts/{key} answers 404 problem+json for an archived, unknown, non-contracts or badly encoded key', async () => {
  const { catalog, url } = await setup();
  await five(catalog);
  for (const path of ['contracts%3Agone', 'contracts%3Amissing', 'review%3Ax', '%E0']) {
    const res = await fetch(`${url}/contracts/${path}`);
    assert.equal(res.status, 404, path);
    assert.equal(res.headers.get('content-type'), 'application/problem+json', path);
    assert.deepEqual(await res.json(), { type: 'about:blank', title: 'Not Found', status: 404, detail: 'no such contract', error: 'no such contract' }, path);
  }
  assert.equal((await fetch(`${url}/contracts/contracts%3Aa`)).status, 200);
});

test('both routes answer 503 problem+json while the catalog is unavailable', async () => {
  const { url } = await setup(() => async () => new Response('{}', { status: 500 }));
  for (const path of ['/contracts', '/contracts/contracts%3Aa']) {
    const res = await fetch(`${url}${path}`);
    assert.equal(res.status, 503, path);
    assert.equal(res.headers.get('content-type'), 'application/problem+json', path);
    const body = (await res.json()) as { title: string; detail: string; error: string };
    assert.equal(body.title, 'Service Unavailable');
    assert.match(body.detail, /^the catalog is unavailable: /);
    assert.equal(body.error, body.detail);
  }
});

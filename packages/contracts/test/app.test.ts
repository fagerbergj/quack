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

async function setup() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-contracts-app-'));
  const store = sqliteCatalog(join(dir, 'c.sqlite'), { currency: 'USD' });
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  cleanups.push(async () => { await store.close(); await rm(dir, { recursive: true, force: true }); });
  // The distributor as the app reaches it in process: the Sessions API over the catalog, called as the tailnet.
  const api = sessionsApi(catalog, { deleteSession: async () => 'deleted', answer: async () => true });
  const fetch = inProcessFetch(async (req, res) => void ((await api(req, res, { kind: 'tailnet' })) || res.writeHead(404).end('{}')));
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
  assert.deepEqual(manifest.pages, [{ method: 'GET', path: '/contracts' }], 'the page reaches it through the proxy');
});

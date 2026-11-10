import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { openCatalog } from '@quack/catalog';
import { Artifacts } from '@quack/sdk';
import { createSessionServer, openSession } from '@quack/session';
import { createDistributor, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';

const dataDir = await mkdtemp(join(tmpdir(), 'quack-export-'));
after(() => rm(dataDir, { recursive: true, force: true }));

// The catalog the session channels would have filled.
const catalog = openCatalog({ dataDir: await mkdtemp(join(tmpdir(), 'quack-export-catalog-')) });
after(() => catalog.close());

async function sessionDir(key: string) {
  await catalog.publish(key, [{ seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'quack', createdAt: 0 } }]);
  const dir = join(dataDir, sessionDirName(key));
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, 'session-key'), key);
  await writeFile(join(dir, 'agent'), 'quack');
  return dir;
}

// A real driver over the data dir whose launches are counted; `live` names the sessions whose process is up.
async function distributor(live: Set<string>, url = 'http://127.0.0.1:1') {
  const launches: string[] = [];
  const driver: HostDriver = { ...processDriver({ command: [], dataDir }), running: () => [...live], ensure: async (key) => (launches.push(key), url) };
  const server = createDistributor(driver, { catalog }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const exported = (key: string) => fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent(key)}/export`);
  return { launches, exported, close: () => new Promise((r) => server.close(r)) };
}

test('a stopped session exports from its store without a launch, byte for byte what its process serves', async () => {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('the answer')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await sessionDir('kept'), models, agent: { model: { provider: m.provider, modelId: m.id } } });
  await session.submit('the question', 'r-1', true);
  const sealed = { kind: 'test.note', version: 1, hash: 'h', content: 'note', sealedAt: 1, verdict: null };
  await session.root.commit(async (tx) => void ((await tx.doc(Artifacts, session.root.id)).kinds['test.note'] = { versions: [sealed] }), ctx);
  const proc = createSessionServer(session).listen(0);
  await new Promise((r) => proc.once('listening', r));
  const live = new Set(['kept']);
  const d = await distributor(live, `http://127.0.0.1:${(proc.address() as AddressInfo).port}`);
  try {
    const fromProcess = await (await d.exported('kept')).text();
    await new Promise((r) => proc.close(r));
    await session.close();
    live.clear();
    d.launches.length = 0;
    const res = await d.exported('kept');
    const fromStore = await res.text();
    assert.equal(res.status, 200);
    assert.deepEqual(d.launches, [], 'exporting a stopped session starts nothing');
    assert.equal(fromStore, fromProcess);
    const body = JSON.parse(fromStore) as { transcript: Array<{ kind: string }>; artifacts: unknown[] };
    assert.deepEqual([body.transcript.map((e) => e.kind), body.artifacts], [['pi.user', 'pi.assistant'], [sealed]]);
  } finally {
    await d.close();
  }
});

test('an unreadable store is a 502 and a missing session a 404, both problem+json, with no launch', async () => {
  await writeFile(join(await sessionDir('broken'), 'session.sqlite'), 'not a database');
  const d = await distributor(new Set());
  try {
    for (const [key, status] of [['broken', 502], ['gone', 404]] as const) {
      const res = await d.exported(key);
      assert.deepEqual([res.status, res.headers.get('content-type')], [status, 'application/problem+json'], key);
    }
    assert.deepEqual(d.launches, []);
  } finally {
    await d.close();
  }
});

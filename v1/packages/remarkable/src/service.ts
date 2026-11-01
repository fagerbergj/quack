import { timingSafeEqual } from 'node:crypto';
import { request } from 'node:http';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { RmClient } from './client.ts';
import { openStore } from './store.ts';

export type ServiceConfig = {
  /** shortcut: one shared bearer token authenticates callers until workload tokens (A12) land. */
  serviceToken: string;
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  distributorUrl: string;
  /** The promoted agent that runs `remarkable.ingest`. */
  agent: string;
  client: Pick<RmClient, 'tree' | 'documents'>;
  now?: () => Date;
  /** Called after each document's answer is recorded; lets tests wait on it instead of polling. */
  onSettle?: (batch: string, document: string) => void;
};

const MAX_BODY = 1 << 20;
const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const expected = Buffer.from(`Bearer ${token}`);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

async function readJson(req: IncomingMessage): Promise<unknown> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY) throw new Error('body too large');
    chunks.push(chunk);
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

export function createRemarkableService(config: ServiceConfig) {
  const store = openStore(config.dbPath, config.now ?? (() => new Date()));
  const inflight = new Set<Promise<void>>();
  const aborts = new Set<() => void>();
  let stopped = false;

  // node:http, not fetch: undici gives up on response headers after 300s, and a vision-model ingest can run longer.
  const post = (url: string, body: string) =>
    new Promise<number>((resolve, reject) => {
      const req = request(url, { method: 'POST', headers: { 'content-type': 'application/json' } }, (res) => (res.resume(), res.on('end', () => resolve(res.statusCode ?? 0))));
      aborts.add(req.destroy.bind(req));
      req.on('error', reject).end(body);
    });

  // The version is the request id, so a replay (a later batch, or a resubmit after restart) joins the existing run.
  async function submit(batch: string, doc: { id: string; name: string; version: string }): Promise<void> {
    const key = encodeURIComponent(`remarkable:${doc.id}`);
    const body = JSON.stringify({ input: JSON.stringify({ id: doc.id, name: doc.name, version: doc.version }), requestId: doc.version, wait: true });
    try {
      const status = await post(`${config.distributorUrl}/sessions/${key}/invocations?agent=${encodeURIComponent(config.agent)}`, body);
      store.settle(batch, doc.id, status >= 200 && status < 300 ? undefined : `distributor answered ${status}`);
    } catch (err) {
      if (stopped) return;
      store.settle(batch, doc.id, `distributor unreachable: ${(err as Error).message}`);
    }
    config.onSettle?.(batch, doc.id);
  }

  function track(batch: string, doc: { id: string; name: string; version: string }) {
    const p = submit(batch, doc).finally(() => inflight.delete(p));
    inflight.add(p);
  }

  async function ingest(req: IncomingMessage): Promise<[number, unknown]> {
    const ids = ((await readJson(req)) as { ids?: unknown } | null)?.ids;
    if (!Array.isArray(ids) || ids.length === 0 || !ids.every((i) => typeof i === 'string' && i.length > 0)) return [400, { error: 'ids must be a non-empty array of document ids' }];
    const wanted = [...new Set(ids as string[])];
    const listed = new Map((await config.client.documents()).map((d) => [d.id, d]));
    const missing = wanted.filter((id) => !listed.has(id));
    if (missing.length) return [404, { error: 'unknown document ids', ids: missing }];
    const docs = wanted.map((id) => listed.get(id)!);
    const batch = store.create(docs.map((d) => ({ document: d.id, name: d.name, version: d.version })));
    docs.forEach((doc) => track(batch, doc));
    return [202, { batch }];
  }

  async function route(req: IncomingMessage): Promise<[number, unknown]> {
    const path = new URL(req.url ?? '', 'http://x').pathname;
    if (req.method === 'GET' && path === '/documents') return [200, { documents: await config.client.tree() }];
    if (req.method === 'POST' && path === '/ingest') return ingest(req);
    const m = req.method === 'GET' ? /^\/batches\/([^/]+)$/.exec(path) : null;
    if (m) {
      const batch = store.get(decodeURIComponent(m[1]));
      return batch ? [200, batch] : [404, { error: 'no such batch' }];
    }
    return [404, { error: 'not found' }];
  }

  const server = createServer(async (req, res) => {
    if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
    try {
      send(res, ...(await route(req)));
    } catch (err) {
      const bad = err instanceof SyntaxError || (err as Error).message === 'body too large';
      send(res, bad ? 400 : 502, { error: (err as Error).message });
    }
  });
  for (const p of store.pending()) track(p.batch, { id: p.document, name: p.name, version: p.version });
  return Object.assign(server, {
    /** Resolves when every submission started so far has been answered. */
    settled: async () => void (await Promise.all(inflight)),
    stop: async () => {
      stopped = true;
      aborts.forEach((abort) => abort());
      server.close();
      await Promise.all(inflight);
      store.close();
    },
  });
}

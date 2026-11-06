import type { IncomingMessage, ServerResponse } from 'node:http';
import { invocationRequest, sendProblem, serviceHttp, tooLarge, type App } from '@quack/sdk';
import type { RmClient, RmEntry } from './client.ts';
import { createIngestQueue, INGEST_CONCURRENCY, type QueuedItem } from './queue.ts';
import { openStore } from './store.ts';

export type ServiceConfig = {
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  distributorUrl: string;
  /** Reaches the distributor; in the distributor's process it is an in-process call. */
  fetch?: typeof fetch;
  /** The promoted agent that runs `remarkable.ingest`. */
  agent: string;
  client: Pick<RmClient, 'tree' | 'documents' | 'downloadPdf' | 'downloadRmdoc'>;
  now?: () => Date;
  /** Called after each document's answer is recorded; lets tests wait on it instead of polling. */
  onSettle?: (batch: string, document: string) => void;
  /** Backoff between resubmits of a wait the distributor dropped; tests inject an instant one. */
  sleep?: (ms: number) => Promise<void>;
  /** Documents ingesting at once; the rest wait in the store. */
  concurrency?: number;
};

const MAX_BACKOFF_MS = 60_000;

const { send, json: readJson } = serviceHttp();

// A 200 only means the session answered; an unanswered result (an operator abort, say) is not an ingested document.
function failure(res: { status: number; text: string }): string | undefined {
  if (res.status < 200 || res.status >= 300) return `distributor answered ${res.status}`;
  try {
    const result = JSON.parse(res.text) as { status?: string; reason?: string; detail?: string };
    // detail carries a failed workflow's error message; the reason alone is just "failed".
    return result.status === 'unanswered' ? `session unanswered: ${result.reason ?? 'no reason'}${result.detail ? `: ${result.detail}` : ''}` : undefined;
  } catch {
    return 'distributor answer was not JSON';
  }
}

// A gateway error or reset means the hold broke, not that the run did; the same request id joins it again.
const dropped = (res: { status: number }) => res.status === 502 || res.status === 504;
const isReset = (err: unknown) => ['ECONNRESET', 'UND_ERR_SOCKET'].includes((err as { cause?: { code?: string } }).cause?.code ?? '');

// Only rmfakecloud failures are the upstream's fault; store errors and the like are this service's own.
const upstream = (err: unknown) => err instanceof Error && (err.message.startsWith('rmfakecloud') || err.message === 'fetch failed');

export function createRemarkableService(config: ServiceConfig): App & { settled(): Promise<void>; stop(): Promise<void> } {
  const store = openStore(config.dbPath, config.now ?? (() => new Date()));
  const aborts = new Set<() => void>();
  let stopped = false;

  // Aborting a held request is how stop() cuts the wait; an in-process fetch has no socket to time out, and tests inject a real URL.
  const post = (url: string, body: string) => {
    const ac = new AbortController();
    const abort = () => ac.abort();
    aborts.add(abort);
    return (config.fetch ?? fetch)(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body, signal: ac.signal })
      .then(async (res) => ({ status: res.status, text: await res.text() }))
      .finally(() => aborts.delete(abort));
  };

  const sleep = config.sleep ?? ((ms: number) => new Promise<void>((resolve) => {
    const t = setTimeout(done, ms);
    function done() { clearTimeout(t); aborts.delete(done); resolve(); }
    aborts.add(done);
  }));

  // The version is the request id, so a replay (a later batch, or a resubmit after restart) joins the existing run.
  async function submit({ batch, document: id, name, version, attempt }: QueuedItem): Promise<void> {
    const { url, body } = invocationRequest(config.distributorUrl, { key: `remarkable:${id}`, agent: config.agent, input: JSON.stringify({ id, name, version }), requestId: attempt ? `${version}#${attempt}` : version, title: name, wait: true });
    try {
      for (let n = 0; ; n++) {
        if (stopped) return;
        const res = await post(url, body).catch((err) => (isReset(err) ? { status: 502, text: '' } : Promise.reject(err)));
        if (stopped) return;
        if (!dropped(res)) {
          store.settle(batch, id, failure(res));
          break;
        }
        await sleep(Math.min(1000 * 2 ** n, MAX_BACKOFF_MS));
      }
    } catch (err) {
      if (stopped) return;
      store.settle(batch, id, `distributor unreachable: ${(err as Error).message}`);
    }
    config.onSettle?.(batch, id);
  }

  const queue = createIngestQueue(store, submit, config.concurrency ?? INGEST_CONCURRENCY);

  // A notebook's ingest state for the page: never ingested, edited since, still running, failed, or current.
  function stateOf(d: RmEntry) {
    const n = store.newest(d.id);
    if (!n) return { state: 'new' };
    if (n.version !== d.version) return { state: 'changed' };
    if (n.status === 'failed') return { state: 'failed', ...(n.error ? { error: n.error } : {}) };
    return { state: n.status === 'pending' ? 'ingesting' : 'current' };
  }
  const withState = (es: RmEntry[]): unknown[] => es.map((e) => (e.folder ? { ...e, children: withState(e.children ?? []) } : { ...e, ...stateOf(e) }));

  // Failed documents retry on their own; `retry: true` instead refuses any whose last attempt did not fail.
  async function ingest(req: IncomingMessage): Promise<[number, unknown]> {
    const body = (await readJson(req)) as { ids?: unknown; retry?: unknown } | null;
    const ids = body?.ids;
    if (!Array.isArray(ids) || ids.length === 0 || !ids.every((i) => typeof i === 'string' && i.length > 0)) return [400, { error: 'ids must be a non-empty array of document ids' }];
    const wanted = [...new Set(ids as string[])];
    const listed = new Map((await config.client.documents()).map((d) => [d.id, d]));
    const missing = wanted.filter((id) => !listed.has(id));
    if (missing.length) return [404, { error: 'unknown document ids', ids: missing }];
    // A retry is a new attempt at the same version, so the workflow starts a fresh run instead of joining the failed one.
    const jobs = wanted.map((id) => {
      const d = listed.get(id)!;
      const last = store.last(d.id, d.version);
      return { id: d.id, name: d.name, version: d.version, attempt: last?.status === 'failed' ? last.attempt + 1 : body?.retry === true ? -1 : (last?.attempt ?? 0) };
    });
    const refused = jobs.filter((j) => j.attempt < 0).map((j) => j.id);
    if (refused.length) return [409, { error: 'only documents whose last attempt failed can be retried', ids: refused }];
    const batch = store.create(jobs.map((j) => ({ document: j.id, name: j.name, version: j.version, attempt: j.attempt })));
    queue.pump();
    return [202, { batch }];
  }

  async function addTopic(req: IncomingMessage): Promise<[number, unknown]> {
    const name = String(((await readJson(req)) as { name?: unknown } | null)?.name ?? '').trim().replace(/\s+/g, ' ');
    if (!name || name.length > 40) return [400, { error: 'name must be 1 to 40 characters' }];
    store.addTopic(name);
    return [200, { topics: store.topics() }];
  }

  async function route(req: IncomingMessage): Promise<[number, unknown]> {
    const path = new URL(req.url ?? '', 'http://x').pathname;
    if (req.method === 'GET' && path === '/') return [200, { tree: withState(await config.client.tree()) }];
    if (req.method === 'GET' && path === '/documents') return [200, { documents: await config.client.tree() }];
    if (req.method === 'POST' && path === '/ingest') return ingest(req);
    if (path === '/topics') return req.method === 'POST' ? addTopic(req) : [200, { topics: store.topics() }];
    const m = req.method === 'GET' ? /^\/batches\/([^/]+)$/.exec(path) : null;
    if (m) {
      const batch = store.get(decodeURIComponent(m[1]));
      return batch ? [200, batch] : [404, { error: 'no such batch' }];
    }
    return [404, { error: 'not found' }];
  }

  async function handle(req: IncomingMessage, res: ServerResponse) {
    try {
      const file = req.method === 'GET' ? /^\/documents\/([^/]+)\/(pdf|rmdoc)$/.exec(new URL(req.url ?? '', 'http://x').pathname) : null;
      if (file) {
        // Download before writing headers: a failure after writeHead would make the catch's send throw and kill the process.
        const id = decodeURIComponent(file[1]);
        const body = file[2] === 'pdf' ? await config.client.downloadPdf(id) : await config.client.downloadRmdoc(id);
        return void res.writeHead(200, { 'content-type': file[2] === 'pdf' ? 'application/pdf' : 'application/zip' }).end(body);
      }
      send(res, ...(await route(req)));
    } catch (err) {
      if (tooLarge(err)) return sendProblem(res, 413, 'body too large');
      const bad = err instanceof SyntaxError || err instanceof URIError;
      send(res, bad ? 400 : upstream(err) ? 502 : 500, { error: (err as Error).message });
    }
  }
  queue.pump();
  return {
    handle,
    /** Resolves when every queued submission has been answered. */
    settled: () => queue.drained(),
    stop: async () => {
      stopped = true;
      queue.stop();
      aborts.forEach((abort) => abort());
      await queue.drained();
      store.close();
    },
  };
}

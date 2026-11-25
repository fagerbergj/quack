import { randomUUID } from 'node:crypto';
import type { Principal } from '@quack/sdk';
import { MAX_FRAME, parseClient, requestIdOf, unrefTimer, type SetTimer } from '@quack/session';
import WebSocket, { WebSocketServer, type RawData } from 'ws';
import type { Outbox, OutboxRow } from './outbox.ts';

const RETRY_MAX_MS = 30_000;
const ANSWER_TIMEOUT_MS = 30_000;
// The id a page sees for an outbox row; the session's own queued rows carry its inbox ids.
const ROW = 'queued:';

type Queued = { id: string; text: string };
type Answer = { type: 'accepted' | 'error'; error?: string };
type Page = { send(frame: object): void; queued(): void };
/** Where a session's socket answers and the headers that open it for a principal; starts the process when it has none. */
export type OpenSession = (key: string, by?: Principal) => Promise<{ url: string; headers: Record<string, string> }>;
export type OutboxDeps = { outbox: Outbox; open: OpenSession; setTimer?: SetTimer; log?: (line: string) => void };

// A session's frame, or undefined for one that is not JSON: a sandboxed session must not be able to throw in the distributor.
function parsed(data: RawData): { type?: string; requestId?: string; error?: string; items?: unknown } | undefined {
  try {
    return JSON.parse(String(data)) ?? undefined;
  } catch {
    return undefined;
  }
}

// Only frames that start as an answer or a queued list are parsed, so a streaming run's view frames pass untouched.
const startsAs = (data: RawData, type: string) => Buffer.isBuffer(data) && data.subarray(0, type.length + 10).toString() === `{"type":"${type}"`;

function opened(url: string, headers: Record<string, string>): Promise<WebSocket> {
  const ws = new WebSocket(url, { headers });
  return new Promise((resolve, reject) => ws.once('open', () => resolve(ws.on('error', () => {}))).once('error', reject));
}

// Sends one row and settles with the session's answer for its requestId; a close, a failed send or silence rejects, and the row stays queued.
function answered(ws: WebSocket, row: OutboxRow, setTimer: SetTimer): Promise<Answer> {
  return new Promise((resolve, reject) => {
    const done = (settle: () => void) => { cancel(); ws.off('message', onMessage).off('close', onClose); settle(); };
    const fail = (why: string | Error) => done(() => reject(why instanceof Error ? why : new Error(why)));
    const onMessage = (data: RawData) => {
      const frame = startsAs(data, 'accepted') || startsAs(data, 'error') ? parsed(data) : undefined;
      if (frame?.requestId === row.requestId) done(() => resolve(frame as Answer));
    };
    const onClose = () => fail('the session process ended');
    const cancel = setTimer(() => fail('the session did not answer a queued message'), ANSWER_TIMEOUT_MS);
    ws.on('message', onMessage).on('close', onClose);
    ws.send(row.frame, (err) => err && fail(err));
  });
}

/** Accepts page sockets for the distributor itself, under the session's own frame budget. */
export const pageSockets = () => new WebSocketServer({ noServer: true, maxPayload: MAX_FRAME, perMessageDeflate: { threshold: 1024 } });

/** Inputs sent while a session has no process: kept in the outbox, shown on every page of the session, and delivered in order,
 * each once the session answered the one before. The session takes a re-sent requestId once, so a retry cannot double one. */
export function sessionOutbox(d: OutboxDeps) {
  const setTimer = d.setTimer ?? unrefTimer;
  const pages = new Map<string, Set<Page>>();
  const draining = new Set<string>();
  const failures = new Map<string, number>();
  const inFlight = new Map<string, string>();
  const rowsOf = (key: string): Queued[] => d.outbox.rows(key).map((r) => ({ id: ROW + r.requestId, text: r.text }));
  const changed = (key: string) => pages.get(key)?.forEach((p) => p.queued());

  function drain(key: string) {
    if (draining.has(key)) return;
    draining.add(key);
    void deliver(key).then(() => failures.delete(key), (err) => retry(key, err)).finally(() => draining.delete(key));
  }

  function retry(key: string, err: unknown) {
    const n = (failures.get(key) ?? 0) + 1;
    failures.set(key, n);
    if (n === 1) d.log?.(`session ${key}: queued messages not delivered (${err instanceof Error ? err.message : String(err)}); retrying with backoff`);
    setTimer(() => drain(key), Math.min(1000 * 2 ** (n - 1), RETRY_MAX_MS));
  }

  // One socket per run of rows from the same principal, since a socket's principal is fixed when it opens.
  async function deliver(key: string) {
    for (let head = d.outbox.rows(key)[0]; head; head = d.outbox.rows(key)[0]) {
      const { url, headers } = await d.open(key, head.by);
      const ws = await opened(url, headers);
      try {
        await deliverAs(key, ws, JSON.stringify(head.by ?? null));
      } finally {
        ws.close();
      }
    }
  }

  async function deliverAs(key: string, ws: WebSocket, by: string) {
    for (let row = d.outbox.rows(key)[0]; row && JSON.stringify(row.by ?? null) === by; row = d.outbox.rows(key)[0]) {
      inFlight.set(key, row.requestId);
      const answer = await answered(ws, row, setTimer).finally(() => inFlight.delete(key));
      d.outbox.remove(key, row.requestId);
      if (answer.type === 'error') pages.get(key)?.forEach((p) => p.send({ type: 'error', error: answer.error }));
      changed(key);
    }
  }

  /** Serves a page socket on a session with no process, or with inputs still in the outbox: it opens on the session's recorded frames,
   * queues inputs until the outbox is empty, starts the process, and relays both ways once it answers. A failed start is retried while the page stays. */
  function serve(page: WebSocket, key: string, who: Principal | undefined, frames: object[]) {
    let session: WebSocket | undefined;
    let recorded: Queued[] = [];
    let failed = 0;
    let cancelRetry = () => {};
    // Reads and the session's own withdraws that arrive before it answers; they need no durability.
    const early: Array<[RawData, boolean]> = [];
    const send = (frame: object) => page.readyState === page.OPEN && page.send(JSON.stringify(frame));
    const self: Page = { send, queued: () => send({ type: 'queued', items: [...recorded, ...rowsOf(key)] }) };
    const listeners = pages.get(key) ?? pages.set(key, new Set()).get(key)!;
    listeners.add(self);

    for (const frame of frames as Array<{ type?: string; items?: Queued[] }>) {
      if (frame.type !== 'queued') send(frame);
      else (recorded = frame.items ?? []), self.queued();
    }

    function queue(input: Extract<ReturnType<typeof parseClient>, { type: 'input' }>) {
      const requestId = input.requestId ?? randomUUID();
      const frame = JSON.stringify({ type: 'input', input: input.input, images: input.images.length ? input.images : undefined, whenBusy: input.whenBusy, requestId });
      const refused = d.outbox.add(key, { requestId, frame, text: input.input, by: who });
      if (refused) return send({ type: 'error', error: refused, requestId: input.requestId });
      send({ type: 'accepted', requestId: input.requestId, queued: true });
      changed(key);
      drain(key);
    }

    // A row on its way cannot be taken back; one already delivered has left the outbox, so this page's list just catches up.
    function unqueue(kind: 'withdraw' | 'steer', requestId: string) {
      if (kind === 'steer') return send({ type: 'error', error: 'Queued messages go to the session as soon as it starts.' });
      if (inFlight.get(key) === requestId) return send({ type: 'error', error: 'That message is already on its way to the session.' });
      if (d.outbox.remove(key, requestId)) return changed(key);
      self.queued();
    }

    function fromPage(data: RawData, binary: boolean) {
      if (session && !d.outbox.pending(key)) return session.send(data, { binary });
      const input = parseClient(String(data));
      if (typeof input === 'string') return send({ type: 'error', error: input, requestId: requestIdOf(String(data)) });
      if (input.type === 'input') return queue(input);
      if ((input.type === 'withdraw' || input.type === 'steer') && input.id.startsWith(ROW)) return unqueue(input.type, input.id.slice(ROW.length));
      if (session) session.send(data, { binary });
      else early.push([data, binary]);
    }

    function fromSession(data: RawData, binary: boolean) {
      if (!d.outbox.pending(key) || !startsAs(data, 'queued')) return page.send(data, { binary });
      const items = parsed(data)?.items;
      recorded = Array.isArray(items) ? items : [];
      self.queued();
    }

    function retryStart(err: unknown) {
      if (page.readyState !== page.OPEN) return;
      const kept = d.outbox.pending(key) ? 'your queued messages are kept; ' : '';
      send({ type: 'error', error: `the session did not start: ${err instanceof Error ? err.message : String(err)}; ${kept}retrying` });
      cancelRetry = setTimer(() => void connect(), Math.min(1000 * 2 ** failed++, RETRY_MAX_MS));
    }

    async function connect() {
      try {
        const { url, headers } = await d.open(key, who);
        const up = await opened(url, headers);
        if (page.readyState !== page.OPEN) return up.close();
        session = up;
        up.on('message', fromSession).on('close', () => ((session = undefined), page.close()));
        for (const [data, binary] of early.splice(0)) up.send(data, { binary });
      } catch (err) {
        retryStart(err);
      }
    }

    page.on('message', fromPage).on('error', () => {});
    page.on('close', () => {
      listeners.delete(self);
      if (!listeners.size) pages.delete(key);
      cancelRetry();
      session?.close();
    });
    void connect();
  }

  return { serve, drain, pending: (key: string) => d.outbox.pending(key) };
}

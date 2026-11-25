import { randomUUID } from 'node:crypto';
import type { Principal } from '@quack/sdk';
import { MAX_FRAME, parseClient, requestIdOf, unrefTimer, type SetTimer } from '@quack/session';
import WebSocket, { WebSocketServer, type RawData } from 'ws';
import type { Outbox, OutboxRow } from './outbox.ts';
import { AgentError } from './process-driver.ts';

const RETRY_MAX_MS = 30_000;
const ANSWER_TIMEOUT_MS = 30_000;
// Failed starts are retried this long before the rows are marked failed for the person to retry or discard.
const GIVE_UP_MS = 30 * 60_000;
// Reads and withdraws a page sends before its session answers; past this the oldest go, as a stale read is worth nothing.
const EARLY_MAX = 32;
// The id a page sees for an outbox row; the session's own queued rows carry its inbox ids.
const ROW = 'queued:';

type Queued = { id: string; text: string; failed?: string };
type Frame = { type?: string; requestId?: string; error?: string; status?: string; reason?: string; items?: unknown };
type Page = { send(frame: object): void; queued(): void };
/** Where a session's socket answers and the headers that open it for a principal; starts the process when it has none.
 * Rejects with AgentError 404 once the session is gone, and with another AgentError when it can never start. */
export type OpenSession = (key: string, by?: Principal) => Promise<{ url: string; headers: Record<string, string> }>;
export type OutboxDeps = { outbox: Outbox; open: OpenSession; setTimer?: SetTimer; now?: () => number; log?: (line: string) => void };

// A session's frame, or undefined for one that is not JSON: a sandboxed session must not be able to throw in the distributor.
function parsed(data: RawData): Frame | undefined {
  try {
    return JSON.parse(String(data)) ?? undefined;
  } catch {
    return undefined;
  }
}

// A page's queued list is merged only on frames that start as one, so a streaming run's view frames pass unparsed. A session that
// serialized `type` later would show its list without the outbox rows until they change; delivery parses every frame, so it cannot miss.
const startsAs = (data: RawData, type: string) => Buffer.isBuffer(data) && data.subarray(0, type.length + 10).toString() === `{"type":"${type}"`;
const clip = (text: string) => (text.length > 60 ? `${text.slice(0, 59)}…` : text);
const messageOf = (err: unknown) => (err instanceof Error ? err.message : String(err));

function opened(url: string, headers: Record<string, string>): Promise<WebSocket> {
  const ws = new WebSocket(url, { headers });
  return new Promise((resolve, reject) => ws.once('open', () => resolve(ws.on('error', () => {}))).once('error', reject));
}

// Sends one row and settles with the session's answer for its requestId; a close, a failed send or silence rejects, and the row stays queued.
function answered(ws: WebSocket, row: OutboxRow, setTimer: SetTimer): Promise<Frame> {
  return new Promise((resolve, reject) => {
    const done = (settle: () => void) => { cancel(); ws.off('message', onMessage).off('close', onClose); settle(); };
    const fail = (why: string | Error) => done(() => reject(why instanceof Error ? why : new Error(why)));
    const onMessage = (data: RawData) => {
      const frame = parsed(data);
      if (frame && frame.requestId === row.requestId && (frame.type === 'accepted' || frame.type === 'error')) done(() => resolve(frame));
    };
    const onClose = () => fail('the session process ended');
    const cancel = setTimer(() => fail('the session did not answer a queued message'), ANSWER_TIMEOUT_MS);
    ws.on('message', onMessage).on('close', onClose);
    ws.send(row.frame, (err) => err && fail(err));
  });
}

// Why the session did not take a delivered row: its refusal, or a declined or aborted submission; undefined when it took it.
const refusal = (a: Frame) => (a.type === 'error' ? a.error ?? 'refused' : a.status === 'unanswered' ? a.reason ?? 'not answered' : undefined);

/** Accepts page sockets for the distributor itself, under the session's own frame budget. */
export const pageSockets = () => new WebSocketServer({ noServer: true, maxPayload: MAX_FRAME, perMessageDeflate: { threshold: 1024 } });

/** Inputs sent while a session has no process: kept in the outbox, shown on every page of the session, and delivered in order,
 * each once the session answered the one before. The session takes a re-sent requestId once, so a retry cannot double one. */
export function sessionOutbox(d: OutboxDeps) {
  const setTimer = d.setTimer ?? unrefTimer;
  const now = d.now ?? Date.now;
  const pages = new Map<string, Set<Page>>();
  const flushing = new Set<string>();
  const failures = new Map<string, { n: number; since: number }>();
  const inFlight = new Map<string, string>();
  const rowsOf = (key: string): Queued[] => d.outbox.rows(key).map((r) => ({ id: ROW + r.requestId, text: r.text, ...(r.failed !== undefined && { failed: r.failed }) }));
  const changed = (key: string) => pages.get(key)?.forEach((p) => p.queued());
  const tell = (key: string, frame: object) => pages.get(key)?.forEach((p) => p.send(frame));
  const next = (key: string) => d.outbox.rows(key).find((r) => r.failed === undefined);

  /** Delivers the session's waiting rows unless a delivery is already running for it. */
  function flush(key: string) {
    if (flushing.has(key)) return;
    flushing.add(key);
    void deliver(key).then(() => failures.delete(key), (err) => retry(key, err)).finally(() => flushing.delete(key));
  }

  // A session that is gone takes its rows with it; one that can never start, or has failed to for GIVE_UP_MS, leaves them failed.
  function retry(key: string, err: unknown) {
    if (err instanceof AgentError && err.status === 404) return void discard(key);
    const { n, since } = failures.get(key) ?? { n: 0, since: now() };
    if (err instanceof AgentError || now() - since >= GIVE_UP_MS) return giveUp(key, messageOf(err));
    failures.set(key, { n: n + 1, since });
    if (n === 0) d.log?.(`session ${key}: queued messages not delivered (${messageOf(err)}); retrying with backoff`);
    setTimer(() => flush(key), Math.min(1000 * 2 ** n, RETRY_MAX_MS));
  }

  function giveUp(key: string, reason: string) {
    failures.delete(key);
    d.outbox.fail(key, reason);
    d.log?.(`session ${key}: gave up delivering queued messages: ${reason}`);
    changed(key);
  }

  // One socket per run of rows from the same principal, since a socket's principal is fixed when it opens.
  async function deliver(key: string) {
    for (let head = next(key); head; head = next(key)) {
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
    for (let row = next(key); row && JSON.stringify(row.by ?? null) === by; row = next(key)) {
      inFlight.set(key, row.requestId);
      const answer = await answered(ws, row, setTimer).finally(() => inFlight.delete(key));
      d.outbox.remove(key, row.requestId);
      const refused = refusal(answer);
      if (refused) tell(key, { type: 'error', error: `Couldn't send "${clip(row.text)}": ${refused}` });
      changed(key);
    }
  }

  /** Discards a session's rows, as its delete does; answers how many there were. */
  function discard(key: string) {
    const n = d.outbox.clear(key);
    failures.delete(key);
    changed(key);
    return n;
  }

  /** Serves a page socket on a session with no process, or with rows in the outbox: it opens on the session's recorded frames,
   * queues inputs while rows wait, starts the process, and relays both ways once it answers. A failed start is retried while the page stays. */
  function serve(page: WebSocket, key: string, who: Principal | undefined, frames: object[]) {
    let session: WebSocket | undefined;
    let recorded: Queued[] = [];
    let failed = 0;
    let dropped = false;
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
      flush(key);
    }

    // Send now on a failed row retries it. A row on its way cannot be taken back; one already delivered has left the outbox, so the list catches up.
    function unqueue(kind: 'withdraw' | 'steer', requestId: string) {
      if (kind === 'steer' && d.outbox.retry(key, requestId)) return void (changed(key), flush(key));
      if (kind === 'steer') return send({ type: 'error', error: 'Queued messages go to the session as soon as it starts.' });
      if (inFlight.get(key) === requestId) return send({ type: 'error', error: 'That message is already on its way to the session.' });
      if (d.outbox.remove(key, requestId)) return changed(key);
      self.queued();
    }

    function hold(data: RawData, binary: boolean) {
      if (early.push([data, binary]) <= EARLY_MAX) return;
      early.shift();
      if (!dropped) send({ type: 'error', error: 'Earlier requests were dropped while the session starts.' });
      dropped = true;
    }

    function fromPage(data: RawData, binary: boolean) {
      if (session && !d.outbox.pending(key)) return session.send(data, { binary });
      const input = parseClient(String(data));
      if (typeof input === 'string') return send({ type: 'error', error: input, requestId: requestIdOf(String(data)) });
      if (input.type === 'input') return queue(input);
      if ((input.type === 'withdraw' || input.type === 'steer') && input.id.startsWith(ROW)) return unqueue(input.type, input.id.slice(ROW.length));
      if (session) session.send(data, { binary });
      else hold(data, binary);
    }

    function fromSession(data: RawData, binary: boolean) {
      if (!startsAs(data, 'queued') || !d.outbox.rows(key).length) return page.send(data, { binary });
      const items = parsed(data)?.items;
      recorded = Array.isArray(items) ? items : [];
      self.queued();
    }

    // A session that is gone or can never start is not retried; the person sees why.
    function retryStart(err: unknown) {
      if (page.readyState !== page.OPEN) return;
      if (err instanceof AgentError) return send({ type: 'error', error: `the session cannot start: ${err.message}` });
      const kept = d.outbox.pending(key) ? 'your queued messages are kept; ' : '';
      send({ type: 'error', error: `the session did not start: ${messageOf(err)}; ${kept}retrying` });
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

  return { serve, flush, discard, held: (key: string) => d.outbox.rows(key).length > 0 };
}

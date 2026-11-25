import { MAX_FRAME, parseClient, requestIdOf, unrefTimer, type SetTimer } from '@quack/session';
import WebSocket, { WebSocketServer, type RawData } from 'ws';

const RETRY_MAX_MS = 30_000;

type Queued = { id: string; text: string };
// A page frame held until the process answers; an input carries the requestId its ack is matched by, and its row in `queued`.
type Held = { raw: string; requestId?: string; row?: Queued };

export type StartingOptions = {
  /** The frames read from the session's store, sent as the socket opens. */
  frames: object[];
  /** Starts the session's process; answers its socket's URL and the headers to open it with. */
  start: () => Promise<{ url: string; headers: Record<string, string> }>;
  setTimer?: SetTimer;
};

// A session's frame, or undefined for one that is not JSON: a sandboxed session must not be able to throw in the distributor.
function parsed(data: RawData): { type?: string; requestId?: string; items?: Queued[] } | undefined {
  try {
    return JSON.parse(String(data)) ?? undefined;
  } catch {
    return undefined;
  }
}

/** Accepts page sockets for the distributor itself, under the session's own frame budget. */
export const pageSockets = () => new WebSocketServer({ noServer: true, maxPayload: MAX_FRAME, perMessageDeflate: { threshold: 1024 } });

/** A page socket on a session with no process: it opens on the session's recorded frames, takes inputs at once into the `queued` list,
 * starts the process, delivers them in order once it answers, then relays both ways. A start that fails is retried while the page stays. */
export function serveStarting(page: WebSocket, o: StartingOptions): void {
  const setTimer = o.setTimer ?? unrefTimer;
  const held: Held[] = [];
  let session: WebSocket | undefined;
  let recorded: Queued[] = [];
  let failures = 0;
  let ack: ((delivered: boolean) => void) | undefined;
  let cancelRetry = () => {};
  let local = 0;

  const send = (frame: object) => page.readyState === page.OPEN && page.send(JSON.stringify(frame));
  const showQueued = () => send({ type: 'queued', items: [...recorded, ...held.flatMap((h) => (h.row ? [h.row] : []))] });

  for (const frame of o.frames) {
    if ((frame as { type?: string }).type === 'queued') recorded = (frame as { items: Queued[] }).items;
    send(frame);
  }

  function hold(raw: string) {
    const input = parseClient(raw);
    if (typeof input === 'string') return send({ type: 'error', error: input, requestId: requestIdOf(raw) });
    const at = input.type === 'withdraw' ? held.findIndex((h, i) => h.row?.id === input.id && !(ack && i === 0)) : -1;
    if (at >= 0) return void (held.splice(at, 1), showQueued());
    if (input.type !== 'input') return void held.push({ raw });
    const requestId = input.requestId ?? `starting-${(local += 1)}`;
    held.push({ raw: JSON.stringify({ ...JSON.parse(raw), requestId }), requestId, row: { id: `starting:${requestId}`, text: input.input } });
    send({ type: 'accepted', requestId: input.requestId, queued: true });
    showQueued();
  }

  // Held frames go one at a time, each input after the session answered the one before, so they arrive in order; an input the
  // session never answered stays at the head and is sent again under its requestId, which the session takes once.
  async function deliver() {
    while (held.length && session && !ack) {
      const next = held[0]!;
      const to = session;
      const delivered = next.requestId ? await new Promise<boolean>((r) => ((ack = r), to.send(next.raw))) : (to.send(next.raw), true);
      ack = undefined;
      if (!delivered) return;
      held.shift();
      showQueued();
    }
  }

  function fromSession(data: RawData, binary: boolean) {
    const frame = held.length ? parsed(data) : undefined;
    if (!frame) return page.send(data, { binary });
    if (frame.type === 'queued') return void ((recorded = Array.isArray(frame.items) ? frame.items : []), showQueued());
    const answers = ack && frame.requestId === held[0]?.requestId && (frame.type === 'accepted' || frame.type === 'error');
    // The page already holds an ack for a held input; only the session's refusal of it is news.
    if (!answers || frame.type === 'error') page.send(data, { binary });
    if (answers) ack!(true);
  }

  function retry(err: unknown) {
    if (page.readyState !== page.OPEN) return;
    const kept = held.some((h) => h.row) ? 'your queued messages are kept; ' : '';
    send({ type: 'error', error: `the session did not start: ${err instanceof Error ? err.message : String(err)}; ${kept}retrying` });
    cancelRetry = setTimer(() => void connect(), Math.min(1000 * 2 ** failures++, RETRY_MAX_MS));
  }

  async function connect() {
    try {
      const { url, headers } = await o.start();
      const up = new WebSocket(url, { headers });
      await new Promise((resolve, reject) => up.once('open', resolve).once('error', reject));
      if (page.readyState !== page.OPEN) return up.close();
      attach(up);
    } catch (err) {
      retry(err);
    }
  }

  function attach(up: WebSocket) {
    session = up;
    failures = 0;
    up.on('message', fromSession).on('error', () => {});
    // A session that ends with frames still held starts again; otherwise the page reconnects, as it would on a proxied socket.
    up.on('close', () => {
      session = undefined;
      ack?.(false);
      if (held.length) retry(new Error('the session process ended'));
      else page.close();
    });
    void deliver();
  }

  page.on('message', (data, binary) => (session && !held.length ? session.send(data, { binary }) : (hold(String(data)), session && void deliver())));
  page.on('close', () => (cancelRetry(), session?.close()));
  void connect();
}

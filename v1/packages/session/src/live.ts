import type { IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { watchEvents } from '@earendil-works/pi-durable';
import { MAX_IMAGE_CHARS, MAX_IMAGES, parseImages } from './images.ts';
import type { ImageContent } from '@earendil-works/pi-ai';
import { failedRows, Failures, pendingQuestions, Questions } from '@quack/sdk';
import { WebSocketServer, type WebSocket } from 'ws';
import type { Session } from './runtime.ts';

// The same budget as an HTTP invocation body. JSON escapes grow a character up to six (\u0000), so maxPayload allows
// for that and every in-budget input reaches the polite length check.
const MAX_INPUT = 1 << 20;

type ClientInput = { type: 'input'; input: string; images: ImageContent[]; requestId?: string; whenBusy?: 'steer' | 'followUp' } | { type: 'withdraw' | 'steer'; id: string };

function parseClient(data: string): ClientInput | string {
  let msg: unknown;
  try {
    msg = JSON.parse(data);
  } catch {
    return 'message must be JSON';
  }
  const { type, id } = (msg ?? {}) as Record<string, unknown>;
  if (type === 'withdraw' || type === 'steer') return typeof id === 'string' && id ? { type, id } : 'id must be a queued message id';
  if (type !== 'input') return 'type must be input, withdraw or steer';
  return parseInput(msg as Record<string, unknown>);
}

function parseInput({ input, images, requestId, whenBusy }: Record<string, unknown>): ClientInput | string {
  const attached = parseImages(images);
  if (typeof attached === 'string') return attached;
  if (typeof input !== 'string' || (input.length === 0 && !attached.length)) return 'input must be a non-empty string';
  if (input.length > MAX_INPUT) return 'input is too long';
  if (requestId !== undefined && typeof requestId !== 'string') return 'requestId must be a string';
  if (whenBusy !== undefined && whenBusy !== 'steer' && whenBusy !== 'followUp') return 'whenBusy must be steer or followUp';
  return { type: 'input', input, images: attached, requestId, whenBusy };
}

// Open questions and failures raise no stream event, so their documents ride the socket as a `standing` frame instead of being polled.
async function watchStanding(session: Session, ws: WebSocket) {
  const { harness, root } = session;
  // watchDoc does not create a document, so touch both first; a session that never asked still gets its watch.
  await root.commit(async (tx) => { await tx.doc(Questions, root.id); await tx.doc(Failures, root.id); }, BACKGROUND_CONTEXT);
  const [questions, failures] = await Promise.all([harness.watchDoc(Questions, root.id, BACKGROUND_CONTEXT), harness.watchDoc(Failures, root.id, BACKGROUND_CONTEXT)]);
  const push = () => ws.readyState === ws.OPEN && ws.send(JSON.stringify({ type: 'standing', questions: pendingQuestions(questions?.value), failures: failedRows(failures?.value) }));
  push();
  for (const watch of [questions, failures]) watch?.start(async () => void push());
  ws.on('close', () => void Promise.all([questions?.stop(), failures?.stop()]));
}

// Streams pi-durable's own events (snapshot, then deltas) with no quack vocabulary on top (A9).
async function stream(session: Session, ws: WebSocket) {
  const events = await watchEvents(session.harness, session.root.id, BACKGROUND_CONTEXT);
  ws.send(JSON.stringify(events.snapshot));
  // The queued follow-ups ride the socket in full (the inbox event carries no text), so a reload shows them again.
  const pushQueued = async () => ws.readyState === ws.OPEN && ws.send(JSON.stringify({ type: 'queued', items: await session.queued() }));
  events.start(async (batch) => {
    if (ws.readyState !== ws.OPEN) return;
    ws.send(JSON.stringify({ type: 'events', events: batch }));
    if (batch.some((e) => e.type === 'inbox_update')) await pushQueued();
  });
  await pushQueued();
  ws.on('close', () => void events.stop());
  ws.on('message', async (data) => {
    const input = parseClient(data.toString());
    if (typeof input === 'string') return ws.send(JSON.stringify({ type: 'error', error: input }));
    // A rejection here would be unhandled, and Node ends the process on those, taking every viewer with it.
    try {
      if (input.type !== 'input') return void (await session.withdraw(input.id, input.type === 'steer'));
      const result = await session.submit(input.input, input.requestId, false, input.whenBusy, input.images);
      ws.send(JSON.stringify({ type: 'accepted', ...result }));
    } catch (err) {
      ws.send(JSON.stringify({ type: 'error', error: err instanceof Error ? err.message : String(err) }));
    }
  });
  await watchStanding(session, ws);
}

export function liveUpgrade(session: Session) {
  const wss = new WebSocketServer({ noServer: true, maxPayload: 6 * MAX_INPUT + MAX_IMAGES * MAX_IMAGE_CHARS + 1024 });
  return (req: IncomingMessage, socket: Duplex, head: Buffer) => {
    if (req.url !== '/ws') return socket.destroy();
    wss.handleUpgrade(req, socket, head, (ws) => void stream(session, ws).catch(() => ws.close(1011)));
  };
}

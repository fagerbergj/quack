import type { IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { ConversationTranscript, type TranscriptItem, type TranscriptView } from '@earendil-works/pi-durable';
import { MAX_IMAGE_CHARS, MAX_IMAGES, parseImages } from './images.ts';
import type { ImageContent } from '@earendil-works/pi-ai';
import { Approvals, Artifacts, failedRows, Failures, pendingQuestions, Questions } from '@quack/sdk';
import { WebSocketServer, type WebSocket } from 'ws';
import type { Session } from './runtime.ts';

// The same budget as an HTTP invocation body. JSON escapes grow a character up to six (\u0000), so maxPayload allows
// for that and every in-budget input reaches the polite length check.
const MAX_INPUT = 1 << 20;

type Gap = Extract<TranscriptItem, { kind: 'gap' }>;
type ClientInput = { type: 'input'; input: string; images: ImageContent[]; requestId?: string; whenBusy?: 'steer' | 'followUp' } | { type: 'withdraw' | 'steer'; id: string } | { type: 'earlier' } | { type: 'gap'; gap: Gap };

function parseClient(data: string): ClientInput | string {
  let msg: unknown;
  try {
    msg = JSON.parse(data);
  } catch {
    return 'message must be JSON';
  }
  const { type, id } = (msg ?? {}) as Record<string, unknown>;
  if (type === 'withdraw' || type === 'steer') return typeof id === 'string' && id ? { type, id } : 'id must be a queued message id';
  if (type === 'earlier') return { type };
  if (type === 'gap') return parseGap(msg as Record<string, unknown>);
  if (type !== 'input') return 'type must be input, withdraw, steer, earlier or gap';
  return parseInput(msg as Record<string, unknown>);
}

function parseGap({ after, upTo }: Record<string, unknown>): ClientInput | string {
  if (typeof after !== 'number' || typeof upTo !== 'number') return 'after and upTo must be entry ids';
  return { type: 'gap', gap: { kind: 'gap', after: after as never, upTo: upTo as never } };
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

// The client's id for an input, echoed on its answer so the client matches answers that cross; undefined when the frame has none.
function requestIdOf(data: string): string | undefined {
  try {
    const { requestId } = JSON.parse(data);
    return typeof requestId === 'string' ? requestId : undefined;
  } catch {
    return undefined;
  }
}

// Open questions, approvals, failures and sealed artifacts raise no stream event, so their documents ride the socket instead of being polled:
// what waits on a person as a `standing` frame and the artifacts as their own, each sent when its documents change.
async function watchStanding(session: Session, ws: WebSocket) {
  const { harness, root } = session;
  // watchDoc does not create a document, so touch each first; a session that never asked still gets its watch.
  await root.commit(async (tx) => { await tx.doc(Questions, root.id); await tx.doc(Failures, root.id); await tx.doc(Approvals, root.id); await tx.doc(Artifacts, root.id); }, BACKGROUND_CONTEXT);
  const [questions, failures, approvals, artifacts] = await Promise.all([
    harness.watchDoc(Questions, root.id, BACKGROUND_CONTEXT), harness.watchDoc(Failures, root.id, BACKGROUND_CONTEXT),
    harness.watchDoc(Approvals, root.id, BACKGROUND_CONTEXT), harness.watchDoc(Artifacts, root.id, BACKGROUND_CONTEXT),
  ]);
  const send = (frame: object) => ws.readyState === ws.OPEN && ws.send(JSON.stringify(frame));
  const standing = () => send({
    type: 'standing', questions: pendingQuestions(questions?.value), failures: failedRows(failures?.value),
    approvals: Object.entries(approvals?.value?.items ?? {}).filter(([, a]) => !a.verdict).map(([id, a]) => ({ id, tool: a.tool, args: a.args, requestedAt: a.requestedAt })),
  });
  const ledger = () => send({ type: 'artifacts', artifacts: Object.values(artifacts?.value?.kinds ?? {}).flatMap((k) => k.versions) });
  standing();
  ledger();
  for (const watch of [questions, failures, approvals]) watch?.start(async () => void standing());
  artifacts?.start(async () => void ledger());
  ws.on('close', () => void Promise.all([questions?.stop(), failures?.stop(), approvals?.stop(), artifacts?.stop()]));
}

// Streams pi-durable's conversation view (A9): the transcript items when they change, the live, usage and inbox state on every change.
async function stream(session: Session, ws: WebSocket) {
  const { root } = session;
  const transcript = new ConversationTranscript((query, limit, c) => root.entries(query, limit, undefined, c));
  let sentItems: unknown;
  let inbox: string | undefined;
  // The queued follow-ups ride the socket in full, so a reload shows them again; the frame follows each inbox change.
  const pushQueued = async () => ws.readyState === ws.OPEN && ws.send(JSON.stringify({ type: 'queued', items: await session.queued() }));
  const render = async (view: TranscriptView) => {
    if (ws.readyState !== ws.OPEN) return;
    const { items, docs } = view;
    ws.send(JSON.stringify({ type: 'view', items: items === sentItems ? undefined : items, live: docs['pi.live'] ?? {}, usage: docs['pi.usage'] }));
    sentItems = items;
    const queue = JSON.stringify(docs['pi.inbox'] ?? null);
    if (queue !== inbox) {
      inbox = queue;
      await pushQueued();
    }
  };
  const watch = await root.watch(BACKGROUND_CONTEXT);
  await render(transcript.merge(watch.value));
  watch.start(async (call) => { if (call.kind === 'value') await render(transcript.merge(call.value)); });
  ws.on('close', () => void watch.stop());
  ws.on('message', async (data) => {
    const input = parseClient(data.toString());
    const requestId = requestIdOf(data.toString());
    if (typeof input === 'string') return ws.send(JSON.stringify({ type: 'error', error: input, requestId }));
    // A rejection here would be unhandled, and Node ends the process on those, taking every viewer with it.
    try {
      if (input.type === 'earlier') return void (await render(await transcript.loadEarlier(BACKGROUND_CONTEXT)));
      if (input.type === 'gap') return void (await render(await transcript.fillGap(input.gap, BACKGROUND_CONTEXT)));
      if (input.type !== 'input') return void (await session.withdraw(input.id, input.type === 'steer'));
      const result = await session.submit(input.input, input.requestId, false, input.whenBusy, input.images);
      ws.send(JSON.stringify({ type: 'accepted', ...result, requestId }));
    } catch (err) {
      ws.send(JSON.stringify({ type: 'error', error: err instanceof Error ? err.message : String(err), requestId }));
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

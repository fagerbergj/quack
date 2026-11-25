import type { IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { ConversationTranscript, type TranscriptItem, type TranscriptView } from '@earendil-works/pi-durable';
import { MAX_IMAGE_CHARS, MAX_IMAGES, parseImages } from './images.ts';
import type { ImageContent } from '@earendil-works/pi-ai';
import { Approvals, Artifacts, failedRows, Failures, modelWait, pendingQuestions, type Approval, type ModelWait, type Principal, Questions, type SealedArtifact } from '@quack/sdk';
import { WebSocketServer, type WebSocket } from 'ws';
import { readStore } from './export.ts';
import { queuedOf, type Caller, type Session } from './runtime.ts';

// The same budget as an HTTP invocation body. JSON escapes grow a character up to six (\u0000), so maxPayload allows
// for that and every in-budget input reaches the polite length check.
const MAX_INPUT = 1 << 20;
/** The largest frame a page may send on its socket. */
export const MAX_FRAME = 6 * MAX_INPUT + MAX_IMAGES * MAX_IMAGE_CHARS + 1024;

type Gap = Extract<TranscriptItem, { kind: 'gap' }>;
type ClientInput = { type: 'input'; input: string; images: ImageContent[]; requestId?: string; whenBusy?: 'steer' | 'followUp' } | { type: 'withdraw' | 'steer'; id: string } | { type: 'earlier' } | { type: 'gap'; gap: Gap };

export function parseClient(data: string): ClientInput | string {
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
export function requestIdOf(data: string): string | undefined {
  try {
    const { requestId } = JSON.parse(data);
    return typeof requestId === 'string' ? requestId : undefined;
  } catch {
    return undefined;
  }
}

// The frames a page socket opens with, built alike by a live stream and from a stopped session's store.
type StandingDocs = { questions?: Parameters<typeof pendingQuestions>[0]; failures?: Parameters<typeof failedRows>[0]; approvals?: Readonly<{ items: Record<string, Approval> }> | null };
const viewFrame = ({ items, docs }: TranscriptView, sent?: unknown) => ({ type: 'view', items: items === sent ? undefined : items, live: docs['pi.live'] ?? {}, usage: docs['pi.usage'] });
const standingFrame = ({ questions, failures, approvals }: StandingDocs) => ({
  type: 'standing', questions: pendingQuestions(questions), failures: failedRows(failures),
  approvals: Object.entries(approvals?.items ?? {}).filter(([, a]) => !a.verdict && !a.withdrawn).map(([id, a]) => ({ id, tool: a.tool, args: a.args, requestedAt: a.requestedAt, ...(a.approving && { approving: true }), ...(a.checks && { checks: a.checks }) })),
});
// Only a kind's newest version carries content; a reader loads an older one on demand over the Sessions API.
const artifactsFrame = (doc: Readonly<{ kinds: Record<string, { versions: SealedArtifact[] }> }> | null | undefined) => ({ type: 'artifacts', artifacts: Object.values(doc?.kinds ?? {}).flatMap((k) => k.versions.map(({ content, ...row }, i) => (i === k.versions.length - 1 ? { ...row, content } : row))) });

/** The frames a page socket on the session stored in `dir` opens with, read from a copy of its store so no process need run; none for a session that never ran. */
export async function snapshotFrames(dir: string): Promise<object[]> {
  const frames = await readStore(dir, async (harness, root) => {
    const watch = await root.watch(BACKGROUND_CONTEXT);
    const view = new ConversationTranscript((query, limit, c) => root.entries(query, limit, undefined, c)).merge(watch.value);
    await watch.stop();
    const [questions, failures, approvals, artifacts] = await Promise.all([harness.snapshot(Questions, root.id, BACKGROUND_CONTEXT), harness.snapshot(Failures, root.id, BACKGROUND_CONTEXT), harness.snapshot(Approvals, root.id, BACKGROUND_CONTEXT), harness.snapshot(Artifacts, root.id, BACKGROUND_CONTEXT)]);
    const queued = (await queuedOf(harness, root)).map(({ id, text }) => ({ id, text }));
    return [viewFrame(view), { type: 'queued', items: queued }, standingFrame({ questions, failures, approvals }), artifactsFrame(artifacts)];
  });
  return frames ?? [];
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
  const standing = () => send(standingFrame({ questions: questions?.value, failures: failures?.value, approvals: approvals?.value }));
  const ledger = () => send(artifactsFrame(artifacts?.value));
  standing();
  ledger();
  for (const watch of [questions, failures, approvals]) watch?.start(async () => void standing());
  artifacts?.start(async () => void ledger());
  ws.on('close', () => void Promise.all([questions?.stop(), failures?.stop(), approvals?.stop(), artifacts?.stop()]));
}

// Streams pi-durable's conversation view: the transcript items when they change, the live, usage and inbox state on every change.
async function stream(session: Session, ws: WebSocket, caller: Caller) {
  const { root } = session;
  const transcript = new ConversationTranscript((query, limit, c) => root.entries(query, limit, undefined, c));
  let sentItems: unknown;
  let inbox: string | undefined;
  // The queued follow-ups ride the socket in full, so a reload shows them again; the frame follows each inbox change.
  const pushQueued = async () => ws.readyState === ws.OPEN && ws.send(JSON.stringify({ type: 'queued', items: await session.queued() }));
  const render = async (view: TranscriptView) => {
    if (ws.readyState !== ws.OPEN) return;
    ws.send(JSON.stringify(viewFrame(view, sentItems)));
    sentItems = view.items;
    const queue = JSON.stringify(view.docs['pi.inbox'] ?? null);
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
      if (input.type !== 'input') return void (await session.withdraw(input.id, input.type === 'steer', caller));
      const result = await session.submit(input.input, input.requestId, false, input.whenBusy, input.images, typeof caller === 'object' ? caller : undefined);
      ws.send(JSON.stringify({ type: 'accepted', ...result, requestId }));
    } catch (err) {
      ws.send(JSON.stringify({ type: 'error', error: err instanceof Error ? err.message : String(err), requestId }));
    }
  });
  // Queued for a model slot rides its own frame, a full snapshot like the rest.
  const sendWait = (waiting: ModelWait | null) => ws.readyState === ws.OPEN && ws.send(JSON.stringify({ type: 'waiting', waiting }));
  sendWait(modelWait.get());
  const unsubscribe = modelWait.subscribe(sendWait);
  ws.on('close', unsubscribe);
  await watchStanding(session, ws);
}

export function liveUpgrade(session: Session) {
  // The artifacts frame carries each kind's newest document, a long contract's hundreds of kilobytes; deflate takes about 4x off it.
  const wss = new WebSocketServer({ noServer: true, maxPayload: MAX_FRAME, perMessageDeflate: { threshold: 1024 } });
  return (req: IncomingMessage, socket: Duplex, head: Buffer, who?: Principal) => {
    if (req.url !== '/ws') return socket.destroy();
    wss.handleUpgrade(req, socket, head, (ws) => void stream(session, ws, who ?? 'service').catch(() => ws.close(1011)));
  };
}

import type { IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { watchEvents } from '@earendil-works/pi-durable';
import { WebSocketServer, type WebSocket } from 'ws';
import type { Session } from './runtime.ts';

type ClientInput = { type: 'input'; input: string; requestId?: string; whenBusy?: 'steer' | 'followUp' };

function parseClient(data: string): ClientInput | string {
  let msg: unknown;
  try {
    msg = JSON.parse(data);
  } catch {
    return 'message must be JSON';
  }
  const { type, input, requestId, whenBusy } = (msg ?? {}) as Record<string, unknown>;
  if (type !== 'input') return 'type must be input';
  if (typeof input !== 'string' || input.length === 0) return 'input must be a non-empty string';
  if (requestId !== undefined && typeof requestId !== 'string') return 'requestId must be a string';
  if (whenBusy !== undefined && whenBusy !== 'steer' && whenBusy !== 'followUp') return 'whenBusy must be steer or followUp';
  return { type, input, requestId, whenBusy };
}

// Streams pi-durable's own events (snapshot, then deltas) with no quack vocabulary on top (A9).
async function stream(session: Session, ws: WebSocket) {
  const events = await watchEvents(session.harness, session.root.id, BACKGROUND_CONTEXT);
  ws.send(JSON.stringify(events.snapshot));
  events.start(async (batch) => { if (ws.readyState === ws.OPEN) ws.send(JSON.stringify({ type: 'events', events: batch })); });
  ws.on('close', () => void events.stop());
  ws.on('message', async (data) => {
    const input = parseClient(data.toString());
    if (typeof input === 'string') return ws.send(JSON.stringify({ type: 'error', error: input }));
    const result = await session.submit(input.input, input.requestId, false, input.whenBusy);
    ws.send(JSON.stringify({ type: 'accepted', ...result }));
  });
}

export function liveUpgrade(session: Session) {
  const wss = new WebSocketServer({ noServer: true });
  return (req: IncomingMessage, socket: Duplex, head: Buffer) => {
    if (req.url !== '/ws') return socket.destroy();
    wss.handleUpgrade(req, socket, head, (ws) => void stream(session, ws).catch(() => ws.close(1011)));
  };
}

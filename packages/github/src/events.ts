import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendProblem } from '@quack/sdk';
import type { PullEvent } from './pulls.ts';

const MAX_STREAMS = 20;
const RETRY_AFTER_S = 30;
const KEEPALIVE_MS = 15_000;

export type Timers = { setInterval: (fn: () => void, ms: number) => unknown; clearInterval: (t: any) => void };

/** GET /events: the `pull_request` events the store publishes, as text/event-stream (https://html.spec.whatwg.org/multipage/server-sent-events.html). `stop` ends every stream. */
export function pullEventStream(subscribe: (fn: (e: PullEvent) => void) => () => void, timers: Timers = globalThis) {
  const streams = new Set<() => void>();
  function handle(_req: IncomingMessage, res: ServerResponse) {
    if (streams.size >= MAX_STREAMS) {
      res.setHeader('retry-after', String(RETRY_AFTER_S));
      return sendProblem(res, 503, `at most ${MAX_STREAMS} event streams at once`);
    }
    // Subscribed before the headers go out, so a client that reads after `open` cannot miss a write in between.
    const unsubscribe = subscribe((e) => res.write(`event: pull_request\ndata: ${JSON.stringify(e)}\n\n`));
    const beat = timers.setInterval(() => res.write(': keepalive\n\n'), KEEPALIVE_MS);
    const end = () => {
      if (!streams.delete(end)) return;
      unsubscribe();
      timers.clearInterval(beat);
      res.end();
    };
    streams.add(end);
    res.on('close', end);
    // no-transform and X-Accel-Buffering keep proxies from buffering; the first comment sends the headers, which a compressing proxy holds until a body byte.
    res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache, no-transform', 'x-accel-buffering': 'no' });
    res.write(': keepalive\n\n');
  }
  return { handle, stop: () => [...streams].forEach((end) => end()) };
}

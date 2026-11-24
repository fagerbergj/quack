import type { ServerResponse } from 'node:http';

/** A burst of writes inside this window goes out as one event. */
const COALESCE_MS = 250;
// Under the idle timeouts of the proxies in front (Traefik, Authentik), so an idle stream stays open.
const KEEPALIVE_MS = 15_000;

export type SetTimer = (fn: () => void, ms: number) => () => void;
const timer: SetTimer = (fn, ms) => {
  const t = setTimeout(fn, ms);
  return () => clearTimeout(t);
};

/** GET /sessions:watch as text/event-stream: a `hello` with the server's build, then `changed` with the keys of the sessions written since the last one. */
export function watchStream(subscribe: (fn: (key: string) => void) => () => void, opts: { hello: () => object; setTimer?: SetTimer }) {
  const setTimer = opts.setTimer ?? timer;
  const streams = new Set<() => void>();
  function open(res: ServerResponse) {
    const keys = new Set<string>();
    let cancelFlush: (() => void) | undefined;
    const flush = () => {
      cancelFlush = undefined;
      res.write(`event: changed\ndata: ${JSON.stringify({ keys: [...keys] })}\n\n`);
      keys.clear();
    };
    // Subscribed before the headers go out, so a client that reads once the stream opens misses no write in between.
    const unsubscribe = subscribe((key) => {
      keys.add(key);
      cancelFlush ??= setTimer(flush, COALESCE_MS);
    });
    let cancelBeat = () => {};
    const beat = () => void (cancelBeat = setTimer(() => (res.write(': keepalive\n\n'), beat()), KEEPALIVE_MS));
    const end = () => {
      if (!streams.delete(end)) return;
      unsubscribe();
      cancelFlush?.();
      cancelBeat();
      res.end();
    };
    streams.add(end);
    res.on('close', end);
    // no-transform and X-Accel-Buffering keep proxies from buffering; the hello is the first body byte a compressing proxy waits for.
    res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache, no-transform', 'x-accel-buffering': 'no' });
    res.write(`event: hello\ndata: ${JSON.stringify(opts.hello())}\n\n`);
    beat();
  }
  return { open, stop: () => [...streams].forEach((end) => end()) };
}

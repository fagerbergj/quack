import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

/** Binds port 0 and releases it, so the caller can hand the number to a listener started later. */
export function freePort(): Promise<number> {
  return new Promise((ok, fail) => {
    const s = createServer().listen(0, '127.0.0.1', () => {
      const { port } = s.address() as AddressInfo;
      s.close(() => ok(port));
    });
    s.once('error', fail);
  });
}

/** An HTTP server that answers every request with its path; port 0 picks a free one. */
export function startEcho(port = 0): Promise<{ url: string; server: Server }> {
  return new Promise((ok, fail) => {
    const server = createServer((req, res) => res.end(req.url));
    server.once('error', fail);
    server.listen(port, '127.0.0.1', () => ok({ url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, server }));
  });
}

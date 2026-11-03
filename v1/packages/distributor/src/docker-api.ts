import { request } from 'node:http';

export type DockerCall = (method: 'GET' | 'POST' | 'DELETE', path: string, body?: unknown) => Promise<{ status: number; body: unknown }>;

/** `unix:///path` or a bare path is a socket; `tcp://host:port` (a docker-socket-proxy) is TCP. */
export function parseDockerHost(raw: string): { socketPath: string } | { host: string; port: number } {
  if (raw.startsWith('tcp://') || raw.startsWith('http://')) {
    const u = new URL(raw.replace(/^tcp:/, 'http:'));
    return { host: u.hostname, port: Number(u.port || 2375) };
  }
  return { socketPath: raw.replace(/^unix:\/\//, '') };
}

/** The few Docker Engine calls the driver makes; no client library, so the endpoints are exactly the ones a socket proxy must allow. */
export function dockerApi(host: ReturnType<typeof parseDockerHost>): DockerCall {
  return (method, path, body) => new Promise((resolve, reject) => {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    const headers = payload ? { 'content-type': 'application/json', 'content-length': Buffer.byteLength(payload) } : {};
    const req = request({ ...host, method, path, headers }, (res) => {
      const chunks: Buffer[] = [];
      res.on('data', (c: Buffer) => chunks.push(c)).on('end', () => {
        const text = Buffer.concat(chunks).toString();
        let parsed: unknown = text;
        try { parsed = text ? JSON.parse(text) : undefined; } catch { /* a plain-text error body stays text */ }
        resolve({ status: res.statusCode ?? 0, body: parsed });
      }).on('error', reject);
    });
    req.on('error', reject).end(payload);
  });
}

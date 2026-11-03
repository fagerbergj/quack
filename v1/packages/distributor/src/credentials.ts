import { randomBytes } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { forward } from './apps.ts';
import { readBody } from './server.ts';

const TOKEN_TTL_MS = 12 * 3_600_000;

/** A12 for container sessions: long-lived service tokens stay here; a session holds a per-launch token and reaches each service through /svc/<name>. */
export function credentialBroker(opts: { ttlMs?: number; now?: () => number } = {}) {
  const now = opts.now ?? Date.now;
  const services = new Map<string, { url: string; token: string }>();
  const issued = new Map<string, { key: string; exp: number }>();

  const send = (res: ServerResponse, status: number, error: string) => void res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify({ error }));

  return {
    register: (name: string, url: string, token: string) => void services.set(name, { url: url.replace(/\/$/, ''), token }),
    has: (name: string) => services.has(name),
    /** A new opaque token for one launch of the session; it dies on revoke or after the TTL. */
    mint(key: string): string {
      const token = randomBytes(32).toString('hex');
      issued.set(token, { key, exp: now() + (opts.ttlMs ?? TOKEN_TTL_MS) });
      return token;
    },
    revoke: (token: string) => void issued.delete(token),
    async route(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
      const match = /^\/svc\/([^/?]+)(\/[^?]*)?(\?.*)?$/.exec(req.url ?? '');
      if (!match) return false;
      const bearer = /^Bearer (.+)$/.exec(req.headers.authorization ?? '')?.[1];
      const grant = bearer ? issued.get(bearer) : undefined;
      if (!grant || grant.exp <= now()) return send(res, 401, 'unauthorized'), true;
      const service = services.get(match[1]);
      if (!service) return send(res, 404, 'no such service'), true;
      const body = await readBody(req).catch(() => undefined);
      if (!body) return send(res, 413, 'body too large'), true;
      await forward(req, res, new URL(`${service.url}${match[2] ?? ''}${match[3] ?? ''}`), service.token, body);
      return true;
    },
  };
}

export type CredentialBroker = ReturnType<typeof credentialBroker>;

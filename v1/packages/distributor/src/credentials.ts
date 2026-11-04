import { randomBytes } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendProblem } from '@quack/sdk';
import { forward } from './apps.ts';
import { readBody } from './server.ts';

const TOKEN_TTL_MS = 12 * 3_600_000;

/** A12 for container sessions: third-party keys stay here; a session holds a per-launch token and reaches those services through /svc/<name>, and the distributor's own routes with the same token. */
/** The session's token as its client presents it: a bearer token, an x-api-key (Exa), or the password of Basic auth (Langfuse). */
function sessionToken(req: IncomingMessage): string | undefined {
  const auth = req.headers.authorization ?? '';
  const key = req.headers['x-api-key'];
  if (typeof key === 'string') return key;
  const basic = /^Basic (.+)$/.exec(auth)?.[1];
  if (basic) return Buffer.from(basic, 'base64').toString().split(':').slice(1).join(':');
  return /^Bearer (.+)$/.exec(auth)?.[1];
}

export function credentialBroker(opts: { ttlMs?: number; now?: () => number } = {}) {
  const now = opts.now ?? Date.now;
  const services = new Map<string, { url: string; auth: Record<string, string> }>();
  const issued = new Map<string, { key: string; exp: number }>();

  /** The session a current launch token belongs to; undefined for an unknown, revoked or expired one. */
  const sessionOf = (token: string): string | undefined => {
    const grant = issued.get(token);
    return grant && grant.exp > now() ? grant.key : undefined;
  };

  return {
    /** `auth` is the header set sent upstream; a string is a bearer token. */
    register: (name: string, url: string, auth: string | Record<string, string>) => void services.set(name, { url: url.replace(/\/$/, ''), auth: typeof auth === 'string' ? { authorization: `Bearer ${auth}` } : auth }),
    has: (name: string) => services.has(name),
    /** A new opaque token for one launch of the session; it dies on revoke or after the TTL. */
    mint(key: string): string {
      const token = randomBytes(32).toString('hex');
      issued.set(token, { key, exp: now() + (opts.ttlMs ?? TOKEN_TTL_MS) });
      return token;
    },
    revoke: (token: string) => void issued.delete(token),
    sessionOf,
    async route(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
      const match = /^\/svc\/([^/?]+)(\/[^?]*)?(\?.*)?$/.exec(req.url ?? '');
      if (!match) return false;
      const presented = sessionToken(req);
      if (!presented || sessionOf(presented) === undefined) return sendProblem(res, 401, 'unauthorized'), true;
      const service = services.get(match[1]);
      if (!service) return sendProblem(res, 404, 'no such service'), true;
      const body = await readBody(req).catch(() => undefined);
      if (!body) return sendProblem(res, 413, 'body too large'), true;
      await forward(req, res, new URL(`${service.url}${match[2] ?? ''}${match[3] ?? ''}`), service.auth, body);
      return true;
    },
  };
}

export type CredentialBroker = ReturnType<typeof credentialBroker>;

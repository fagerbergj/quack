import { randomBytes } from 'node:crypto';
import { request, type IncomingMessage, type ServerResponse } from 'node:http';
import { sendProblem } from '@quack/sdk';

/** For container sessions: third-party keys stay here; a session holds a per-launch token and reaches those services through /proxy/<name>, and the distributor's own routes with the same token. */
/** The session's token as its client presents it: a bearer token, an x-api-key (Exa), or the password of Basic auth (Langfuse). */
function sessionToken(req: IncomingMessage): string | undefined {
  const auth = req.headers.authorization ?? '';
  const key = req.headers['x-api-key'];
  if (typeof key === 'string') return key;
  const basic = /^Basic (.+)$/.exec(auth)?.[1];
  if (basic) return Buffer.from(basic, 'base64').toString().split(':').slice(1).join(':');
  return /^Bearer (.+)$/.exec(auth)?.[1];
}

/** Streams the request to `target` with the real credentials (`auth` headers) the caller never sees: method, content type and body go up uncapped, and the upstream's status and body come back unchanged. GET, HEAD and DELETE send no body. */
function proxy(req: IncomingMessage, res: ServerResponse, target: URL, auth: Record<string, string>): Promise<void> {
  return new Promise<void>((resolve) => {
    const fail = (err: Error) => {
      // After headers a JSON error would corrupt the partial body, so cut the connection instead.
      if (res.headersSent) res.destroy();
      else sendProblem(res, 502, err.message);
      resolve();
    };
    const headers: Record<string, string> = { ...auth };
    if (req.headers['content-type']) headers['content-type'] = req.headers['content-type'];
    const up = request(target, { method: req.method, headers }, (r) => {
      res.writeHead(r.statusCode ?? 502, { 'content-type': r.headers['content-type'] ?? 'application/json' });
      r.pipe(res).on('finish', resolve);
      r.on('error', fail);
    });
    up.on('error', fail);
    res.on('close', () => up.destroy());
    if (['GET', 'HEAD', 'DELETE'].includes(req.method ?? '')) up.end();
    else req.pipe(up);
  });
}

export function credentialBroker() {
  const services = new Map<string, { url: string; auth: Record<string, string> }>();
  const issued = new Map<string, string>();

  /** The session a current launch token belongs to; undefined for an unknown or revoked one. */
  const sessionOf = (token: string): string | undefined => issued.get(token);

  return {
    /** `auth` is the header set sent upstream; a string is a bearer token. */
    register: (name: string, url: string, auth: string | Record<string, string>) => void services.set(name, { url: url.replace(/\/$/, ''), auth: typeof auth === 'string' ? { authorization: `Bearer ${auth}` } : auth }),
    has: (name: string) => services.has(name),
    /** A new opaque token for one launch of the session; it lives until revoked or the distributor restarts. */
    mint(key: string): string {
      const token = randomBytes(32).toString('hex');
      issued.set(token, key);
      return token;
    },
    revoke: (token: string) => void issued.delete(token),
    sessionOf,
    async route(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
      const match = /^\/proxy\/([^/?]+)(\/[^?]*)?(\?.*)?$/.exec(req.url ?? '');
      if (!match) return false;
      const presented = sessionToken(req);
      if (!presented || sessionOf(presented) === undefined) return sendProblem(res, 401, 'unauthorized'), true;
      const service = services.get(match[1]);
      if (!service) return sendProblem(res, 404, 'no such service'), true;
      await proxy(req, res, new URL(`${service.url}${match[2] ?? ''}${match[3] ?? ''}`), service.auth);
      return true;
    },
  };
}

export type CredentialBroker = ReturnType<typeof credentialBroker>;

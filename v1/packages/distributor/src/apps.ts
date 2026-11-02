import { request, type IncomingMessage, type ServerResponse } from 'node:http';

export type Apps = Record<string, { url: string; tokenEnv: string }>;

// QUACK_APPS is a JSON object of name -> { url, tokenEnv }; tokenEnv names the env var holding that service's bearer token.
export function parseApps(raw: string | undefined, env: NodeJS.ProcessEnv = process.env): Apps {
  if (!raw) return {};
  const apps = JSON.parse(raw) as Apps;
  for (const [name, app] of Object.entries(apps)) {
    if (!app?.url || !app.tokenEnv || !env[app.tokenEnv]) throw new Error(`QUACK_APPS.${name} needs url and a set tokenEnv`);
  }
  return apps;
}

// Forwards /apps/<name>[/...] to the registered service with its bearer token, so a page never holds it.
// The client's own authorization is dropped; method, query, body and status pass through.
export function appsProxy(apps: Apps, env: NodeJS.ProcessEnv = process.env) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const match = /^\/apps\/([^/?]+)(?=[/?]|$)/.exec(req.url ?? '');
    if (!match) return false;
    const app = Object.hasOwn(apps, match[1]) ? apps[match[1]] : undefined;
    if (!app) return res.writeHead(404, { 'content-type': 'application/json' }).end('{"error":"no such app"}'), true;
    await new Promise<void>((resolve) => {
      const fail = (err: Error) => {
        if (!res.headersSent) res.writeHead(502, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: err.message }));
        resolve();
      };
      const headers: Record<string, string> = { authorization: `Bearer ${env[app.tokenEnv]}` };
      if (req.headers['content-type']) headers['content-type'] = req.headers['content-type'];
      const up = request(new URL(req.url!, app.url), { method: req.method, headers }, (r) => {
        res.writeHead(r.statusCode ?? 502, { 'content-type': r.headers['content-type'] ?? 'application/json' });
        r.pipe(res).on('finish', resolve);
        r.on('error', fail);
      });
      up.on('error', fail);
      res.on('close', () => up.destroy());
      req.pipe(up);
    });
    return true;
  };
}

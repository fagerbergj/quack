import { request, type IncomingMessage, type ServerResponse } from 'node:http';
import { extensionDbPath, inProcessFetch, sendProblem, type App, type DeploymentFile, type ExtensionManifest } from '@quack/sdk';

/** Sends the request to `target` with credentials (`auth` headers) the caller never sees; method, content type and body pass through, and so do the status and body back. */
export function forward(req: IncomingMessage, res: ServerResponse, target: URL, auth: Record<string, string>, body: Buffer): Promise<void> {
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
    up.end(body);
  });
}

const sendFailure = (res: ServerResponse, err: unknown) => (res.headersSent ? res.destroy() : sendProblem(res, 500, err instanceof Error ? err.message : String(err)));

const isPage = (app: App, method: string, path: string) => !!app.pages?.some((r) => r.method === method && (typeof r.path === 'string' ? r.path === path : r.path.test(path)));

/** Why a request may not reach the app: a credential-free one needs a declared page route (401), a session needs the app granted (403). */
async function refuse(app: App, name: string, session: string | undefined, method: string, path: string, granted: (session: string) => Promise<ReadonlySet<string>>): Promise<[number, string] | undefined> {
  if (session === undefined) return isPage(app, method, path) ? undefined : [401, 'unauthorized'];
  return (await granted(session)).has(name) ? undefined : [403, 'agent has no grant for this app'];
}

/** Dispatches /apps/<name>[/...] to the mounted app in this process, with the path relative to its mount. A credential-free request reaches only the app's declared page and ingress routes; any other route needs a current launch credential (401), and the session may reach only the apps `granted` lists (403). */
export function appsGateway(apps: Record<string, App>, sessionOf: (token: string) => string | undefined, granted: (session: string) => Promise<ReadonlySet<string>>) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const name = /^\/apps\/([^/?]+)(?=[/?]|$)/.exec(req.url ?? '')?.[1];
    if (!name) return false;
    const presented = /^Bearer (.+)$/.exec(req.headers.authorization ?? '')?.[1];
    const session = presented === undefined ? undefined : sessionOf(presented);
    if (presented !== undefined && session === undefined) return sendProblem(res, 401, 'unauthorized'), true;
    const app = Object.hasOwn(apps, name) ? apps[name] : undefined;
    // URL resolves `..` and `%2e`; a path leaving /apps/<name> would reach another app's routes.
    const target = new URL(req.url!, 'http://x');
    const mount = `/apps/${name}`;
    if (!app || !`${target.pathname}/`.startsWith(`${mount}/`)) return sendProblem(res, 404, 'no such app'), true;
    const path = target.pathname.slice(mount.length) || '/';
    const refusal = await refuse(app, name, session, req.method ?? 'GET', path, granted);
    if (refusal) return sendProblem(res, refusal[0], refusal[1]), true;
    req.url = `${path}${target.search}`;
    await app.handle(req, res).catch((err) => sendFailure(res, err));
    return true;
  };
}

/** Builds the app of every extension the deployment file lists; an unlisted extension is not mounted, and a listed name nothing mounts is refused. Apps reach the distributor and each other through in-process fetches, resolved when called. */
export async function mountApps(deployment: DeploymentFile, manifests: ExtensionManifest[], o: { distributorUrl: string; distributor: (req: IncomingMessage, res: ServerResponse) => unknown; env?: NodeJS.ProcessEnv }): Promise<Record<string, App>> {
  const env = o.env ?? process.env;
  const apps: Record<string, App> = {};
  const peerFetch = (name: string): typeof fetch => (input, init) => (apps[name] ? inProcessFetch((req, res) => apps[name].handle(req, res))(input, init) : Promise.resolve(new Response('{"error":"no such app"}', { status: 404 })));
  const fetch = inProcessFetch(o.distributor);
  for (const [name, entry] of Object.entries(deployment.extensions ?? {})) {
    const manifest = manifests.find((m) => m.name === name);
    if (!manifest?.app) throw new Error(`extensions.${name}: no extension named ${name} mounts an app`);
    const app = await manifest.app({ settings: entry.settings, dbPath: extensionDbPath(deployment, name, env), distributorUrl: o.distributorUrl, fetch, peerFetch, deployment, env });
    apps[name] = Object.assign(app, { pages: manifest.pages });
  }
  return apps;
}

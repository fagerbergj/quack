import { request, type IncomingMessage, type ServerResponse } from 'node:http';
import { extensionDbPath, sendProblem, type App, type DeploymentFile, type ExtensionManifest } from '@quack/sdk';

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

/** Dispatches /apps/<name>[/...] to the mounted app in this process, with the path relative to its mount: a bearer credential must be a current launch one and its session reaches only the apps `granted` lists; none is the page, which passes on the tailnet boundary. */
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
    if (session !== undefined && !(await granted(session)).has(name)) return sendProblem(res, 403, 'agent has no grant for this app'), true;
    req.url = `${target.pathname.slice(mount.length) || '/'}${target.search}`;
    await app.handle(req, res).catch((err) => sendFailure(res, err));
    return true;
  };
}

/** Builds the app of every extension the deployment file lists; an unlisted extension is not mounted, and a listed name nothing mounts is refused. */
export async function mountApps(deployment: DeploymentFile, manifests: ExtensionManifest[], o: { distributorUrl: string; env?: NodeJS.ProcessEnv }): Promise<Record<string, App>> {
  const env = o.env ?? process.env;
  const apps: Record<string, App> = {};
  for (const [name, entry] of Object.entries(deployment.extensions ?? {})) {
    const mount = manifests.find((m) => m.name === name)?.app;
    if (!mount) throw new Error(`extensions.${name}: no extension named ${name} mounts an app`);
    apps[name] = await mount({ settings: entry.settings, dbPath: extensionDbPath(deployment, name, env), distributorUrl: o.distributorUrl, deployment, env });
  }
  return apps;
}

import { rmSync } from 'node:fs';
import { createServer, type IncomingMessage } from 'node:http';
import { isDeepStrictEqual } from 'node:util';
import { dockerApi, parseDockerHost, type DockerCall } from './docker-api.ts';
import { sessionContainers, type SessionContainers } from './host-config.ts';

// The gate stands between the distributor and dockerd, so it imports only node: modules and these two node-only files.
export const SESSION_LABEL = 'quack.session';
export const SESSION_PREFIX = 'quack-session-';
export const SESSION_MOUNT = '/session';
const SESSION_DIR = /^[0-9a-f]{32}$/;
const MAX_BODY = 1 << 20;
const CREATE_KEYS = ['Image', 'Cmd', 'WorkingDir', 'Env', 'Labels', 'HostConfig'];
const HOST_KEYS = ['Runtime', 'NetworkMode', 'Mounts', 'Binds', 'AutoRemove', 'CapDrop', 'SecurityOpt'];

/** The one mount a session container has: its own directory of the data volume, at /session. */
export const sessionMount = (data: SessionContainers['data'], dir: string) => 'volume' in data
  ? { Mounts: [{ Type: 'volume', Source: data.volume, Target: SESSION_MOUNT, VolumeOptions: { Subpath: dir } }] }
  : { Binds: [`${data.hostDir}/${dir}:${SESSION_MOUNT}`] };

class Denied extends Error {
  status: number;
  constructor(reason: string, status = 403) {
    super(reason);
    this.status = status;
  }
}
const deny = (reason: string, status?: number): never => {
  throw new Denied(reason, status);
};
const only = (url: URL, keys: string[]) => [...url.searchParams.keys()].forEach((k) => keys.includes(k) || deny(`query parameter ${k}`));
const extra = (obj: Record<string, unknown>, keys: string[], at: string) => Object.keys(obj).forEach((k) => keys.includes(k) || deny(`${at}${k} is not allowed`));
const isObject = (v: unknown): v is Record<string, any> => typeof v === 'object' && v !== null && !Array.isArray(v);

// An allow-list of keys, so Privileged, CapAdd, Devices, the host namespaces, extra networks and the rest are refused without naming each.
function checkCreate(body: unknown, name: string, p: SessionContainers) {
  if (!isObject(body) || !isObject(body.HostConfig)) return deny('a create needs a JSON body with a HostConfig');
  extra(body, CREATE_KEYS, '');
  if (body.Image !== p.image) deny(`image ${String(body.Image)} is not the session image`);
  const labels = body.Labels;
  if (!isObject(labels) || Object.keys(labels).join() !== SESSION_LABEL || typeof labels[SESSION_LABEL] !== 'string') deny(`Labels must be exactly ${SESSION_LABEL}`);
  const dir = name.startsWith(SESSION_PREFIX) ? name.slice(SESSION_PREFIX.length) : '';
  if (!SESSION_DIR.test(dir)) deny(`name ${name} is not a session container name`);
  checkHost(body.HostConfig, dir, p);
}

function checkHost(host: Record<string, any>, dir: string, p: SessionContainers) {
  extra(host, HOST_KEYS, 'HostConfig.');
  if (host.Runtime !== p.runtime) deny(`runtime ${String(host.Runtime)} is not ${p.runtime}`);
  if (host.NetworkMode !== p.network) deny(`network ${String(host.NetworkMode)} is not ${p.network}`);
  if (!Array.isArray(host.CapDrop) || !host.CapDrop.includes('ALL')) deny('HostConfig.CapDrop must include ALL');
  if (!(host.SecurityOpt ?? []).every((o: unknown) => o === 'no-new-privileges')) deny('HostConfig.SecurityOpt may only hold no-new-privileges');
  if (!isDeepStrictEqual({ Mounts: host.Mounts, Binds: host.Binds }, { Mounts: undefined, Binds: undefined, ...sessionMount(p.data, dir) })) deny('the only mount allowed is the session\'s own data directory');
}

// A label filter in Docker's list form, e.g. {"label":["quack.session"]}, so only session containers are listed.
function checkList(url: URL) {
  only(url, ['all', 'filters']);
  const raw = url.searchParams.get('filters') ?? '';
  let filters: unknown;
  try { filters = JSON.parse(raw); } catch { /* refused below */ }
  const labels = isObject(filters) && Array.isArray(filters.label) ? filters.label : [];
  if (!labels.some((l: unknown) => l === SESSION_LABEL || (typeof l === 'string' && l.startsWith(`${SESSION_LABEL}=`)))) deny(`a list must filter on the ${SESSION_LABEL} label`);
  return `/containers/json?all=${encodeURIComponent(url.searchParams.get('all') ?? '0')}&filters=${encodeURIComponent(raw)}`;
}

// Resolved to the container's id, so the call lands on the container whose label was checked.
async function sessionContainer(api: DockerCall, ref: string) {
  const { status, body } = await api('GET', `/containers/${encodeURIComponent(ref)}/json`);
  const c = body as { Id?: string; Config?: { Labels?: Record<string, string> } } | undefined;
  // The driver deletes a session's old container before each create, so a missing one is routine and answers as dockerd would.
  if (status === 404) deny(`no such container: ${ref}`, 404);
  if (status !== 200 || !c?.Id || c.Config?.Labels?.[SESSION_LABEL] === undefined) deny(`${ref} is not a session container`);
  return c!.Id!;
}

type Call = { method: 'GET' | 'POST' | 'DELETE'; path: string; body?: unknown };
const ACTIONS: Record<string, string[]> = { start: [], stop: ['t'], wait: ['condition'] };

/** The call to make upstream for an allowed request; throws Denied for anything the docker driver does not make. */
export async function admit(api: DockerCall, p: SessionContainers, method: string, url: URL, body: unknown): Promise<Call> {
  const path = url.pathname;
  const [, kind, ref, action] = path.split('/');
  if (method === 'GET' && path === `/networks/${encodeURIComponent(p.network)}`) return only(url, []), { method, path };
  if (method === 'GET' && path === '/containers/json') return { method, path: checkList(url) };
  if (method === 'POST' && path === '/containers/create') {
    const name = url.searchParams.get('name') ?? '';
    only(url, ['name']);
    checkCreate(body, name, p);
    // Rebuilt from the checked values, so a repeated query parameter cannot mean something else to dockerd.
    return { method, path: `${path}?name=${encodeURIComponent(name)}`, body };
  }
  if (kind !== 'containers' || !ref || path.split('/').length > 4) deny(`${method} ${path}`);
  return containerCall(api, method, url, ref!, action);
}

async function containerCall(api: DockerCall, method: string, url: URL, ref: string, action: string | undefined): Promise<Call> {
  if (method === 'DELETE' && action === undefined) return only(url, ['force']), { method, path: `/containers/${await sessionContainer(api, ref)}${url.search}` };
  if (method === 'POST' && action && Object.hasOwn(ACTIONS, action)) return only(url, ACTIONS[action]!), { method, path: `/containers/${await sessionContainer(api, ref)}/${action}${url.search}` };
  return deny(`${method} ${url.pathname}`);
}

async function readBody(req: IncomingMessage) {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const c of req as AsyncIterable<Buffer>) {
    if ((size += c.length) > MAX_BODY) deny('body too large');
    chunks.push(c);
  }
  const text = Buffer.concat(chunks).toString();
  try { return text ? JSON.parse(text) : undefined; } catch { return deny('body is not JSON'); }
}

/** A Docker Engine endpoint that serves only the calls the docker driver makes, for session containers only, and answers 403 to the rest. */
export function dockerGate(p: SessionContainers, api: DockerCall) {
  return createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://gate');
      const call = await admit(api, p, req.method ?? '', url, await readBody(req));
      // Forwarded re-serialized, so dockerd reads the same object the checks read (no duplicate or case-variant keys).
      const up = await api(call.method, call.path, call.body);
      res.writeHead(up.status, up.body === undefined ? {} : { 'content-type': 'application/json' }).end(up.body === undefined ? undefined : typeof up.body === 'string' ? up.body : JSON.stringify(up.body));
    } catch (err) {
      const status = err instanceof Denied ? err.status : 502;
      if (status !== 404) console.warn(`docker gate: ${status === 403 ? 'denied' : 'failed'} ${req.method} ${req.url?.split('?')[0]}: ${(err as Error).message}`);
      if (!res.headersSent) res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify({ message: `docker gate: ${(err as Error).message}` }));
    }
  });
}

// node packages/distributor/src/docker-gate.ts, with the session settings the distributor uses, QUACK_GATE_UPSTREAM and QUACK_GATE_LISTEN (a port or a socket path).
if (import.meta.main) {
  const policy = sessionContainers(process.env);
  const listen = process.env.QUACK_GATE_LISTEN ?? '2375';
  if (!/^\d+$/.test(listen)) rmSync(listen, { force: true });
  dockerGate(policy, dockerApi(parseDockerHost(process.env.QUACK_GATE_UPSTREAM ?? '/var/run/docker.sock')))
    .listen(/^\d+$/.test(listen) ? Number(listen) : listen, () => console.log(`docker gate on ${listen}: ${policy.image} under ${policy.runtime} on ${policy.network}`));
}

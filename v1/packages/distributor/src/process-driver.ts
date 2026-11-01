import { spawn, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cp, mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { dirname, join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

export type AgentStatus = 'draft' | 'promoted';
export type AgentInfo = { name: string; version: number; status: AgentStatus };
export type SessionInfo = { key: string; agent: string; version?: number; running: boolean; lastActivity: string };

/** A request the agent registry refuses: 404 for an unknown agent, 409 for a session that already runs another. */
export class AgentError extends Error {
  status: 404 | 409;
  constructor(status: 404 | 409, message: string) {
    super(message);
    this.status = status;
  }
}

export type HostDriver = {
  /** Starts the session's process if it is not running and returns its base URL. A new session is bound to `agent` (default: the deployment's). */
  ensure(key: string, agent?: string): Promise<string>;
  agents(): Promise<AgentInfo[]>;
  /** Every session on disk, newest first. */
  sessions(): Promise<SessionInfo[]>;
  stop(key: string): Promise<void>;
  /** Records when a stopped session next needs a process (its earliest sleep); undefined clears it. */
  setWake(key: string, at: number | undefined): Promise<void>;
  /** Every session with a recorded wake time. */
  wakes(): Promise<Array<{ key: string; at: number }>>;
  running(): string[];
  pid(key: string): number | undefined;
};

type Child = { proc: ChildProcess; url: string; stopping: boolean };

async function freePort(): Promise<number> {
  const server = createServer().listen(0, '127.0.0.1');
  await new Promise((r) => server.once('listening', r));
  const { port } = server.address() as { port: number };
  await new Promise((r) => server.close(r));
  return port;
}

async function waitHealthy(url: string, proc: ChildProcess, timeoutMs: number) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (proc.exitCode !== null) throw new Error(`session process exited with ${proc.exitCode}`);
    try {
      if ((await fetch(`${url}/ping`)).ok) return;
    } catch {
      // not listening yet
    }
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error('session process did not become healthy');
}

export const sessionDirName = (key: string) => createHash('sha256').update(key).digest('hex').slice(0, 32);

const readText = (path: string) => readFile(path, 'utf8').catch(() => undefined);
const readJson = async (path: string) => JSON.parse((await readText(path)) ?? 'null') as { name?: unknown; version?: unknown; status?: unknown } | null;

// Content hash of a bundle directory (minus its version.lock), so two copies of one agent version can be compared.
export async function digest(dir: string): Promise<string> {
  const files = (await readdir(dir, { recursive: true, withFileTypes: true })).filter((e) => e.isFile() && e.name !== 'version.lock').map((e) => join(e.parentPath, e.name)).sort();
  const hash = createHash('sha256');
  for (const f of files) {
    const bytes = await readFile(f);
    // status is a lifecycle state of the version, not part of its definition (DEP-6).
    const content = f === join(dir, 'agent.json') ? JSON.stringify({ ...JSON.parse(bytes.toString()), status: undefined }) : bytes;
    hash.update(f.slice(dir.length)).update(content);
  }
  return hash.digest('hex');
}

const SNAPSHOT = 'agent-def';

// Newest message time in the session's own log; file mtimes move on every restart and checkpoint.
async function lastActivity(dir: string): Promise<number> {
  let logged = 0;
  try {
    const db = new DatabaseSync(join(dir, 'session.sqlite'), { readOnly: true });
    try {
      const row = db.prepare("SELECT max(json_extract(m.value, '$.timestamp')) AS at FROM entries, json_each(entries.record, '$.model') m").get() as { at: number | null };
      logged = row.at ?? 0;
    } finally {
      db.close();
    }
  } catch {
    // No log yet: fall back to when the session was created.
  }
  return logged || (await stat(join(dir, 'session-key')).then((s) => s.mtimeMs, () => 0));
}

const CRASH_BUDGET = 3;
const CRASH_WINDOW_MS = 60_000;

// Development host driver: one child process per session on this machine (A11). Container drivers replace it in deployment.
export function processDriver(opts: { command: string[]; dataDir: string; agentsDir?: string; defaultAgent?: string; env?: NodeJS.ProcessEnv; startTimeoutMs?: number }): HostDriver {
  const children = new Map<string, Child>();
  const starting = new Map<string, Promise<string>>();
  const crashes = new Map<string, number[]>();

  const sessionDir = (key: string) => join(opts.dataDir, sessionDirName(key));

  // Each agent.json is read fresh, so a new session binds to the newest definition; bind() then snapshots it for that session (DEP-3).
  async function registry(): Promise<Array<AgentInfo & { path: string }>> {
    const dirs = opts.agentsDir ? await readdir(opts.agentsDir, { withFileTypes: true }).catch(() => []) : [];
    const found = await Promise.all(dirs.filter((d) => d.isDirectory()).map(async (d) => {
      const path = join(opts.agentsDir!, d.name, 'agent.json');
      const def = await readJson(path).catch(() => null);
      const status: AgentStatus = def?.status === 'promoted' ? 'promoted' : 'draft';
      return typeof def?.name === 'string' && typeof def.version === 'number' ? { name: def.name, version: def.version, status, path } : undefined;
    }));
    return found.filter((a) => a !== undefined);
  }

  const defaultPath = opts.defaultAgent ?? process.env.QUACK_AGENT;
  async function defaultName() {
    return defaultPath ? ((await readJson(defaultPath).catch(() => null))?.name as string | undefined) ?? '' : '';
  }

  // A version is immutable: refuse a source whose name and version match a session snapshot with different content.
  async function assertUnforked(name: string, source: string) {
    const version = (await readJson(source).catch(() => null))?.version;
    const want = await digest(dirname(source));
    for (const d of await readdir(opts.dataDir).catch(() => [])) {
      const snap = join(opts.dataDir, d, SNAPSHOT);
      const def = await readJson(join(snap, 'agent.json')).catch(() => null);
      if (def?.name === name && def.version === version && (await digest(snap)) !== want) {
        throw new AgentError(409, `agent ${name} version ${version} changed since it was first used; bump its version`);
      }
    }
  }

  // Copies the agent's bundle into the session dir; every later start loads that copy, never the agents dir (DEP-3).
  async function snapshot(name: string, source: string, dir: string, isNew: boolean) {
    if (isNew) await assertUnforked(name, source);
    await cp(dirname(source), join(dir, SNAPSHOT), { recursive: true });
  }

  // A session's agent is fixed by its first request; later requests must agree.
  // shortcut: two first requests naming different agents at once race on the record, last writer wins; serialize per key if that matters.
  async function bind(key: string, requested?: string): Promise<{ name: string; path: string | undefined }> {
    const dir = sessionDir(key);
    const snapshotDir = join(dir, SNAPSHOT);
    const recorded = await readText(join(dir, 'agent'));
    if (recorded !== undefined && requested && requested !== recorded) throw new AgentError(409, `session runs agent ${recorded}, not ${requested}`);
    // A recorded session with no snapshot (pre-upgrade, or a lost copy) deliberately re-pins to the current definition once.
    if (recorded !== undefined && (await stat(snapshotDir).catch(() => null))) return { name: recorded, path: join(snapshotDir, 'agent.json') };
    const fallback = await defaultName();
    const name = recorded ?? requested ?? fallback;
    const source = (await registry()).find((a) => a.name === name)?.path ?? (name === fallback ? defaultPath : undefined);
    if (!source && name) throw new AgentError(404, `unknown agent ${name}`);
    if (source) await snapshot(name, source, dir, recorded === undefined);
    if (recorded === undefined) {
      await mkdir(dir, { recursive: true });
      await writeFile(join(dir, 'session-key'), key);
      await writeFile(join(dir, 'agent'), name);
    }
    return { name, path: source && join(snapshotDir, 'agent.json') };
  }

  async function start(key: string, agentPath: string | undefined): Promise<string> {
    const dir = sessionDir(key);
    const port = await freePort();
    const url = `http://127.0.0.1:${port}`;
    const [cmd, ...args] = opts.command;
    const proc = spawn(cmd, args, {
      env: { ...process.env, ...opts.env, ...(agentPath && { QUACK_AGENT: agentPath }), PORT: String(port), QUACK_SESSION_DIR: dir, QUACK_SESSION_KEY: key },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    const child: Child = { proc, url, stopping: false };
    // A crash after startup restarts from the log (NFR-HAR-2), up to a budget so a process-level crash cannot loop;
    // past it the next request restarts the session. Startup crashes are reported to the caller instead.
    proc.once('exit', () => {
      if (children.get(key) !== child) return;
      children.delete(key);
      if (child.stopping) return;
      const recent = (crashes.get(key) ?? []).filter((t) => Date.now() - t < CRASH_WINDOW_MS).concat(Date.now());
      crashes.set(key, recent);
      if (recent.length <= CRASH_BUDGET) void ensure(key).catch(() => {});
    });
    try {
      await waitHealthy(url, proc, opts.startTimeoutMs ?? 15_000);
    } catch (err) {
      proc.kill('SIGKILL');
      throw err;
    }
    children.set(key, child);
    return url;
  }

  async function ensure(key: string, agent?: string): Promise<string> {
    const { path } = await bind(key, agent);
    const live = children.get(key);
    if (live) return live.url;
    let pending = starting.get(key);
    if (!pending) {
      pending = start(key, path).finally(() => starting.delete(key));
      starting.set(key, pending);
    }
    return pending;
  }

  const agents = async () => (await registry()).map(({ name, version, status }) => ({ name, version, status }));

  async function sessions(): Promise<SessionInfo[]> {
    const dirs = await readdir(opts.dataDir).catch(() => []);
    const all = await Promise.all(dirs.map(async (d) => {
      const key = await readText(join(opts.dataDir, d, 'session-key'));
      if (key === undefined) return undefined;
      const agent = (await readText(join(opts.dataDir, d, 'agent'))) ?? '';
      const version = (await readJson(join(opts.dataDir, d, SNAPSHOT, 'agent.json')).catch(() => null))?.version;
      return { key, agent, ...(typeof version === 'number' && { version }), running: children.has(key), at: await lastActivity(join(opts.dataDir, d)) };
    }));
    return all.filter((s) => s !== undefined).sort((a, b) => b.at - a.at).map(({ at, ...s }) => ({ ...s, lastActivity: new Date(at).toISOString() }));
  }

  async function setWake(key: string, at: number | undefined) {
    if (at === undefined) await rm(join(sessionDir(key), 'wake'), { force: true });
    else await writeFile(join(sessionDir(key), 'wake'), String(at));
  }

  async function wakes() {
    const dirs = await readdir(opts.dataDir).catch(() => []);
    const found = await Promise.all(dirs.map(async (d) => {
      const [key, at] = await Promise.all([readText(join(opts.dataDir, d, 'session-key')), readText(join(opts.dataDir, d, 'wake'))]);
      return key !== undefined && Number.isFinite(Number(at)) && at ? { key, at: Number(at) } : undefined;
    }));
    return found.filter((w) => w !== undefined);
  }

  async function stop(key: string) {
    const child = children.get(key);
    if (!child) return;
    child.stopping = true;
    children.delete(key);
    const exited = new Promise((r) => child.proc.once('exit', r));
    child.proc.kill('SIGTERM');
    await exited;
  }

  return { ensure, agents, sessions, stop, setWake, wakes, running: () => [...children.keys()], pid: (key) => children.get(key)?.proc.pid };
}

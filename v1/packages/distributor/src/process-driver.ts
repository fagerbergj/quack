import { spawn, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, readdir, readFile, stat, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { join } from 'node:path';

export type AgentInfo = { name: string; version: number };
export type SessionInfo = { key: string; agent: string; running: boolean; lastActivity: string };

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
const readJson = async (path: string) => JSON.parse((await readText(path)) ?? 'null') as { name?: unknown; version?: unknown } | null;

async function lastModified(dir: string): Promise<number> {
  const times = await Promise.all(['session.sqlite', 'session.sqlite-wal', 'session-key'].map((f) => stat(join(dir, f)).then((s) => s.mtimeMs, () => 0)));
  return Math.max(...times);
}

const CRASH_BUDGET = 3;
const CRASH_WINDOW_MS = 60_000;

// Development host driver: one child process per session on this machine (A11). Container drivers replace it in deployment.
export function processDriver(opts: { command: string[]; dataDir: string; agentsDir?: string; defaultAgent?: string; env?: NodeJS.ProcessEnv; startTimeoutMs?: number }): HostDriver {
  const children = new Map<string, Child>();
  const starting = new Map<string, Promise<string>>();
  const crashes = new Map<string, number[]>();

  const sessionDir = (key: string) => join(opts.dataDir, sessionDirName(key));

  // Each agent.json is read fresh, so a new session starts on the newest definition while running ones keep their process (DEP-3).
  async function registry(): Promise<Array<AgentInfo & { path: string }>> {
    const dirs = opts.agentsDir ? await readdir(opts.agentsDir, { withFileTypes: true }).catch(() => []) : [];
    const found = await Promise.all(dirs.filter((d) => d.isDirectory()).map(async (d) => {
      const path = join(opts.agentsDir!, d.name, 'agent.json');
      const def = await readJson(path).catch(() => null);
      return typeof def?.name === 'string' && typeof def.version === 'number' ? { name: def.name, version: def.version, path } : undefined;
    }));
    return found.filter((a) => a !== undefined);
  }

  const defaultPath = opts.defaultAgent ?? process.env.QUACK_AGENT;
  async function defaultName() {
    return defaultPath ? ((await readJson(defaultPath).catch(() => null))?.name as string | undefined) ?? '' : '';
  }

  // The session's agent is fixed by its first request and recorded beside session-key; later requests must agree.
  // shortcut: two first requests naming different agents at once race on the record, last writer wins; serialize per key if that matters.
  async function bind(key: string, requested?: string): Promise<{ name: string; path: string | undefined }> {
    const dir = sessionDir(key);
    const recorded = await readText(join(dir, 'agent'));
    if (recorded !== undefined && requested && requested !== recorded) throw new AgentError(409, `session runs agent ${recorded}, not ${requested}`);
    const fallback = await defaultName();
    const name = recorded ?? requested ?? fallback;
    const path = (await registry()).find((a) => a.name === name)?.path ?? (name === fallback ? defaultPath : undefined);
    if (!path && name) throw new AgentError(404, `unknown agent ${name}`);
    if (recorded === undefined) {
      await mkdir(dir, { recursive: true });
      await writeFile(join(dir, 'session-key'), key);
      await writeFile(join(dir, 'agent'), name);
    }
    return { name, path };
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

  const agents = async () => (await registry()).map(({ name, version }) => ({ name, version }));

  async function sessions(): Promise<SessionInfo[]> {
    const dirs = await readdir(opts.dataDir).catch(() => []);
    const all = await Promise.all(dirs.map(async (d) => {
      const key = await readText(join(opts.dataDir, d, 'session-key'));
      if (key === undefined) return undefined;
      const agent = (await readText(join(opts.dataDir, d, 'agent'))) ?? '';
      return { key, agent, running: children.has(key), at: await lastModified(join(opts.dataDir, d)) };
    }));
    return all.filter((s) => s !== undefined).sort((a, b) => b.at - a.at).map(({ at, ...s }) => ({ ...s, lastActivity: new Date(at).toISOString() }));
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

  return { ensure, agents, sessions, stop, running: () => [...children.keys()], pid: (key) => children.get(key)?.proc.pid };
}

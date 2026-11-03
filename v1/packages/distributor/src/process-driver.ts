import { spawn, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { cp, mkdir, mkdtemp, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Readable, Writable } from 'node:stream';
import { childPort, shellEnv } from '@quack/session';

export type AgentStatus = 'draft' | 'promoted';
export type AgentInfo = { name: string; version: number; status: AgentStatus };
export type SessionInfo = { key: string; agent: string; version?: number; retention?: { idleDays: number }; title?: string; archived?: boolean; running: boolean; createdAt: string; lastActivity: string };

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
  /** Every session on disk, newest-created first. */
  sessions(): Promise<SessionInfo[]>;
  stop(key: string): Promise<void>;
  /** Deletes the session's directory (log, workspace, snapshot); false when no such session exists. The caller has checked it is not running. */
  remove(key: string): Promise<boolean>;
  /** Sets the session's title (the `title` file); undefined restores the default from its first message. False when no such session exists. */
  setTitle(key: string, title: string | undefined): Promise<boolean>;
  /** Hides the session from the default list while it stays readable. False when no such session exists. */
  setArchived(key: string, archived: boolean): Promise<boolean>;
  /** Records when a stopped session next needs a process (its earliest sleep); undefined clears it. */
  setWake(key: string, at: number | undefined): Promise<void>;
  /** Records that the session was doing work at `at`, for runs that add no log messages. */
  touch(key: string, at: number): Promise<void>;
  /** Every session with a recorded wake time. */
  wakes(): Promise<Array<{ key: string; at: number }>>;
  running(): string[];
  pid(key: string): number | undefined;
};

type Child = { proc: ChildProcess; url: string; stopping: boolean };

export const sessionDirName = (key: string) => createHash('sha256').update(key).digest('hex').slice(0, 32);

const exists = (path: string) => stat(path).then(() => true, () => false);
const readText = (path: string) => readFile(path, 'utf8').catch(() => undefined);
const readJson = async (path: string) => JSON.parse((await readText(path)) ?? 'null') as { name?: unknown; version?: unknown; status?: unknown; retention?: { idleDays?: unknown } } | null;

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

// The session store records no wall-clock creation time, so `created` is written once with the session.
// Older sessions backfill it once from their first logged message, else the session-key file's mtime.
async function createdAt(dir: string): Promise<number> {
  const recorded = Number(await readText(join(dir, 'created')));
  if (recorded) return recorded;
  const first = (await logTimes(dir, 'min')) || (await stat(join(dir, 'session-key')).then((s) => Math.floor(s.mtimeMs), () => 0));
  if (first) await writeFile(join(dir, 'created'), String(first)).catch(() => undefined);
  return first;
}

async function logTimes(dir: string, fn: 'min' | 'max'): Promise<number> {
  try {
    const db = new DatabaseSync(join(dir, 'session.sqlite'), { readOnly: true });
    try {
      const row = db.prepare(`SELECT ${fn}(json_extract(m.value, '$.timestamp')) AS at FROM entries, json_each(entries.record, '$.model') m`).get() as { at: number | null };
      return row.at ?? 0;
    } finally {
      db.close();
    }
  } catch {
    return 0;
  }
}

// Newest of the last log message and the last recorded touch; file mtimes move on every restart and checkpoint.
async function lastActivity(dir: string): Promise<number> {
  const touched = Number(await readText(join(dir, 'touched'))) || 0;
  return Math.max(await logTimes(dir, 'max'), touched) || createdAt(dir);
}

const TITLE_MAX = 120;

// A title the caller gave on the first submission (the session's `title` file) beats the first message.
function recordedTitle(dir: string): string | undefined {
  try {
    return readFileSync(join(dir, 'title'), 'utf8').trim() || undefined;
  } catch {
    return undefined;
  }
}

// The session's first user message, read straight from its store so listing never wakes a process.
function firstInput(dir: string): string | undefined {
  let db: DatabaseSync | undefined;
  try {
    db = new DatabaseSync(join(dir, 'session.sqlite'), { readOnly: true });
    const row = db.prepare("SELECT json_extract(m.value, '$.content') AS c, json_type(m.value, '$.content') AS t FROM entries e, json_each(e.record, '$.model') m WHERE json_extract(m.value, '$.role') = 'user' ORDER BY e.id LIMIT 1").get() as { c?: string; t?: string } | undefined;
    const parts: unknown = row?.t === 'array' && row.c ? JSON.parse(row.c) : row?.c;
    const text = Array.isArray(parts) ? parts.map((p) => (p?.type === 'text' ? p.text : '')).join(' ') : parts;
    return typeof text === 'string' && text.trim() ? text.trim().replace(/\s+/g, ' ').slice(0, TITLE_MAX) : undefined;
  } catch {
    return undefined;
  } finally {
    db?.close();
  }
}

const OUTPUT_TAIL = 4096;

// A broken sink (closed terminal, dead pipe) must never take the distributor down, so its errors are dropped.
export function forwardOutput(src: Readable, dst: Writable, tap: (b: Buffer) => void) {
  // A dead sink never drains, so the child's pipe is drained into the void rather than left to fill and block it.
  dst.on('error', () => {}).on('close', () => src.resume());
  src.on('data', (b: Buffer) => {
    tap(b);
    if (dst.writable && !dst.write(b)) {
      src.pause();
      dst.once('drain', () => src.resume());
    }
  });
}

type Report = { javascriptStack?: { stack?: string[] }; libuv?: Array<{ type: string; is_active: boolean }> };

// The report is written in place, so an unparseable file is one still being written.
async function readReport(dir: string): Promise<Report | undefined> {
  const file = (await readdir(dir).catch(() => [] as string[])).find((f) => f.endsWith('.json'));
  return file ? (JSON.parse((await readText(join(dir, file))) ?? '') as Report) : undefined;
}

// What a child that never reported a port was doing: a diagnostic report (SIGUSR2) taken before the caller kills it.
async function stuckReport(proc: ChildProcess, dir: string): Promise<string> {
  proc.kill('SIGUSR2');
  let report: Report | undefined;
  for (let i = 0; i < 100 && !report; i++) {
    report = await readReport(dir).catch(() => undefined);
    if (!report) await new Promise((r) => setTimeout(r, 50));
  }
  if (!report) return 'no diagnostic report within 5s';
  const active = (report.libuv ?? []).filter((h) => h.is_active).map((h) => h.type).join(',');
  return `stack: ${report.javascriptStack?.stack?.join(' <- ') ?? 'none'}; active handles: ${active || 'none'}`;
}

const CRASH_BUDGET = 3;
const CRASH_WINDOW_MS = 60_000;

// Development host driver: one child process per session on this machine (A11). Container drivers replace it in deployment.
export function processDriver(opts: { command: string[]; dataDir: string; agentsDir?: string; defaultAgent?: string; /** Variables from the host a session process receives, by its agent's definition path; default: toolchain basics only. */ inherit?: (agentPath: string | undefined) => NodeJS.ProcessEnv | Promise<NodeJS.ProcessEnv>; env?: NodeJS.ProcessEnv; startTimeoutMs?: number }): HostDriver {
  const children = new Map<string, Child>();
  const starting = new Map<string, Promise<string>>();
  const crashes = new Map<string, number[]>();

  // The first message never changes once written, so it is scanned once per session directory; a recorded title can appear later (an existing session's next webhook), so its file is checked on every listing.
  const titles = new Map<string, string>();
  function cachedTitle(dir: string): string | undefined {
    const recorded = recordedTitle(dir)?.slice(0, TITLE_MAX);
    if (recorded) return recorded;
    const known = titles.get(dir);
    if (known !== undefined) return known;
    const found = firstInput(dir);
    if (found !== undefined) titles.set(dir, found);
    return found;
  }

  const sessionDir = (key: string) => join(opts.dataDir, sessionDirName(key));

  // Each agent.json is read fresh, so a new session binds to the newest definition; bind() then snapshots it for that session (DEP-3).
  async function registry(): Promise<Array<AgentInfo & { path: string; retention?: unknown }>> {
    const dirs = opts.agentsDir ? await readdir(opts.agentsDir, { withFileTypes: true }).catch(() => []) : [];
    const found = await Promise.all(dirs.filter((d) => d.isDirectory()).map(async (d) => {
      const path = join(opts.agentsDir!, d.name, 'agent.json');
      const def = await readJson(path).catch(() => null);
      const status: AgentStatus = def?.status === 'promoted' ? 'promoted' : 'draft';
      return typeof def?.name === 'string' && typeof def.version === 'number' ? { name: def.name, version: def.version, status, path, retention: def.retention?.idleDays } : undefined;
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
      await writeFile(join(dir, 'created'), String(Date.now()));
    }
    return { name, path: source && join(snapshotDir, 'agent.json') };
  }

  async function start(key: string, agentPath: string | undefined): Promise<string> {
    const dir = sessionDir(key);
    const [cmd, ...args] = opts.command;
    // The session process gets an allowlist, never the host's whole environment; its shells narrow it further.
    const inherited = await (opts.inherit?.(agentPath) ?? shellEnv(process.env));
    const reportDir = await mkdtemp(join(tmpdir(), 'quack-report-'));
    const env: NodeJS.ProcessEnv = { ...inherited, ...opts.env, ...(agentPath && { QUACK_AGENT: agentPath }), PORT: '0', QUACK_SESSION_DIR: dir, QUACK_SESSION_KEY: key };
    env.NODE_OPTIONS = `${env.NODE_OPTIONS ?? ''} --report-on-signal --report-signal=SIGUSR2 --report-directory=${reportDir}`.trim();
    const proc = spawn(cmd, args, { env, stdio: ['ignore', 'pipe', 'pipe', 'ipc'] });
    // Output still reaches this process's streams; the tail only rides along in a startup failure.
    let tail = '';
    const keep = (b: Buffer) => void (tail = (tail + b).slice(-OUTPUT_TAIL));
    forwardOutput(proc.stdout!, process.stdout, keep);
    forwardOutput(proc.stderr!, process.stderr, keep);
    // The session binds its own port and reports it, so the parent never probes a port another process may own.
    const child: Child = { proc, url: '', stopping: false };
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
      child.url = `http://127.0.0.1:${await childPort(proc, 'session process', opts.startTimeoutMs ?? 15_000)}`;
    } catch (err) {
      const silent = (err as NodeJS.ErrnoException).code === 'E_NO_PORT';
      const detail = silent ? await stuckReport(proc, reportDir) : '';
      proc.kill('SIGKILL');
      if (silent) {
        console.error(`session start failed for ${key}: ${detail}; output: ${tail.trim() || '(none)'}`);
        throw new Error(`${(err as Error).message}; ${detail}`);
      }
      throw err;
    } finally {
      void rm(reportDir, { recursive: true, force: true }).catch(() => {});
    }
    children.set(key, child);
    return child.url;
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
    // Retention is storage policy, so a session pinned before it existed follows its agent's current bundle.
    const current = new Map((await registry()).map((a) => [a.name, a.retention]));
    const all = await Promise.all(dirs.map(async (d) => {
      const key = await readText(join(opts.dataDir, d, 'session-key'));
      if (key === undefined) return undefined;
      const agent = (await readText(join(opts.dataDir, d, 'agent'))) ?? '';
      const pinned = await readJson(join(opts.dataDir, d, SNAPSHOT, 'agent.json')).catch(() => null);
      const { version } = pinned ?? {};
      const idleDays = pinned?.retention?.idleDays ?? current.get(agent);
      const title = cachedTitle(join(opts.dataDir, d));
      return { key, agent, ...(typeof version === 'number' && { version }), ...(typeof idleDays === 'number' && { retention: { idleDays } }), ...(title && { title }), ...((await exists(join(opts.dataDir, d, 'archived'))) && { archived: true }), running: children.has(key), created: await createdAt(join(opts.dataDir, d)), at: await lastActivity(join(opts.dataDir, d)) };
    }));
    return all.filter((s) => s !== undefined).sort((a, b) => b.created - a.created || a.key.localeCompare(b.key)).map(({ at, created, ...s }) => ({ ...s, createdAt: new Date(created).toISOString(), lastActivity: new Date(at).toISOString() }));
  }

  const touch = (key: string, at: number) => writeFile(join(sessionDir(key), 'touched'), String(at));

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

  async function remove(key: string) {
    const dir = sessionDir(key);
    if ((await readText(join(dir, 'session-key'))) === undefined) return false;
    await rm(dir, { recursive: true, force: true });
    return true;
  }

  // A title or flag is a file in the session's directory, like `wake`; setting one never starts a process.
  async function setTitle(key: string, title: string | undefined) {
    if (!(await exists(join(sessionDir(key), 'session-key')))) return false;
    if (title) await writeFile(join(sessionDir(key), 'title'), title);
    else await rm(join(sessionDir(key), 'title'), { force: true });
    return true;
  }

  async function setArchived(key: string, archived: boolean) {
    if (!(await exists(join(sessionDir(key), 'session-key')))) return false;
    if (archived) await writeFile(join(sessionDir(key), 'archived'), '');
    else await rm(join(sessionDir(key), 'archived'), { force: true });
    return true;
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

  return { ensure, agents, sessions, stop, remove, setTitle, setArchived, setWake, touch, wakes, running: () => [...children.keys()], pid: (key) => children.get(key)?.proc.pid };
}

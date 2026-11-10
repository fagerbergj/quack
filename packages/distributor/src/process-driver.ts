import { spawn } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { cp, mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import type { Readable, Writable } from 'node:stream';
import { StringDecoder } from 'node:string_decoder';
import { SESSION_KEY } from '@quack/catalog';
import { childPort, lastActivity, readExport, shellEnv, unrefTimer, type SessionExport, type SetTimer } from '@quack/session';
import type { CredentialBroker } from './credentials.ts';
import { forgetProcess, recordProcess } from './orphans.ts';
import { keepVersion, readBundle, SAFE_NAME, STORE, versionDir, type AgentDetail, type RubricFor } from './agent-bundle.ts';

export type AgentStatus = 'draft' | 'promoted';
export type AgentInfo = { name: string; version: number; status: AgentStatus; model?: string; description?: string };
export type AgentView = AgentDetail & { /** Every version the host holds a copy of, newest first: those it has loaded since the version store began, and those sessions pinned. */ versions: Array<{ version: number; status: AgentStatus }> };

/** A request the agent registry refuses: 404 for an unknown agent, 409 for a switch while a run is active or an agent named on a non-submission request. */
export class AgentError extends Error {
  status: 400 | 404 | 409;
  constructor(status: 400 | 404 | 409, message: string) {
    super(message);
    this.status = status;
  }
}

export type HostDriver = {
  /** Starts the session's process if it is not running and returns its base URL. A new session is bound to `agent` (default: the deployment's). */
  ensure(key: string, agent?: string): Promise<string>;
  /** The agent the session is bound to; undefined for a session that does not exist yet. */
  agentOf(key: string): Promise<string | undefined>;
  /** Binds an existing session to `agent` from its next start; the caller has checked no run is active and stops the process. 404 for an unknown agent. */
  rebind(key: string, agent: string): Promise<void>;
  agents(): Promise<AgentInfo[]>;
  /** One agent's definition, prompt and rubric at `version` (default: current); undefined when the host holds no such copy. */
  agent(name: string, version?: number): Promise<AgentView | undefined>;
  /** Marks the agent's current definition promoted; refused (409) when `version` is not the current one. */
  promote(name: string, version: number): Promise<AgentStatus>;
  /** The agent version the session is pinned to and its retention (the pinned bundle's, else its agent's current one). */
  pinned(key: string): Promise<{ version?: number; retention?: { idleDays: number } }>;
  /** The session's export read from its store on disk, starting nothing; rejects when the store cannot be read. */
  exported(key: string): Promise<SessionExport>;
  /** Stops the session's process; a start in flight is waited out and then stopped. */
  stop(key: string): Promise<void>;
  /** Stops any process the session has, starting ones included, then deletes its directory (log, workspace, snapshot); false when no such session exists. The caller has checked it has no work. */
  remove(key: string): Promise<boolean>;
  /** Records when a stopped session next needs a process (its earliest sleep); undefined clears it. */
  setWake(key: string, at: number | undefined): Promise<void>;
  /** Records that the session was active at `at` (a run completed or is working); the stored value only moves forward. */
  touch(key: string, at: number): Promise<void>;
  /** Every session with a recorded wake time. */
  wakes(): Promise<Array<{ key: string; at: number }>>;
  running(): string[];
  pid(key: string): number | undefined;
  /** The bearer the session's running server requires; undefined when none runs. */
  credential(key: string): string | undefined;
};

/** A session's running process or container, as its launcher reports it. */
export type Launched = { url: string; /** The bearer its server requires, minted for this launch. */ token: string; pid?: number; stop(): Promise<void>; onExit(cb: () => void): void };
/** Starts one session: `dir` is its data directory and `agentPath` its pinned definition. Rejects when it cannot come up. */
export type Launch = (spec: { key: string; dir: string; agentPath: string | undefined }) => Promise<Launched>;

type Child = { launched: Launched; stopping: boolean };

/** Where in its directory a session finds its launch's server token, which it reads and removes at boot. */
export const LAUNCH_TOKEN_FILE = '.launch-token';
export async function writeLaunchToken(dir: string): Promise<string> {
  const token = randomBytes(32).toString('hex');
  await writeFile(join(dir, LAUNCH_TOKEN_FILE), token, { mode: 0o600 });
  return token;
}

export const sessionDirName = (key: string) => createHash('sha256').update(key).digest('hex').slice(0, 32);

/** The agent definition a session was pinned to; the file does not exist for a session with no agent. */
export const pinnedAgentPath = (dataDir: string, key: string) => join(dataDir, sessionDirName(key), SNAPSHOT, 'agent.json');

const exists = (path: string) => stat(path).then(() => true, () => false);
const readText = (path: string) => readFile(path, 'utf8').catch(() => undefined);
const readJson = async (path: string) => JSON.parse((await readText(path)) ?? 'null') as { name?: unknown; version?: unknown; status?: unknown; model?: unknown; description?: unknown; retention?: { idleDays?: unknown } } | null;

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

const OUTPUT_TAIL = 4096;
const LINE_MAX = 64 * 1024;

// A broken sink (closed terminal, dead pipe) must never take the distributor down, so its errors are dropped.
// Every session shares the sink, so each line carries `prefix` (the session key) to be attributable.
export function forwardOutput(src: Readable, dst: Writable, tap: (b: Buffer) => void, prefix = '') {
  // A dead sink never drains, so the child's pipe is drained into the void rather than left to fill and block it.
  const ignore = () => {};
  const drainVoid = () => src.resume();
  dst.on('error', ignore).on('close', drainVoid);
  src.once('close', () => dst.off('error', ignore).off('close', drainVoid));
  const write = (text: string) => {
    if (dst.writable && !dst.write(text)) {
      src.pause();
      dst.once('drain', () => src.resume());
    }
  };
  const decoder = new StringDecoder('utf8');
  let partial = '';
  src.on('data', (b: Buffer) => {
    tap(b);
    const lines = (partial + decoder.write(b)).split('\n');
    partial = lines.pop()!;
    // A line that never ends is cut, so a child cannot grow the buffer without bound.
    if (partial.length > LINE_MAX) {
      lines.push(partial);
      partial = '';
    }
    if (lines.length) write(lines.map((l) => `${prefix}${l}\n`).join(''));
  });
  src.once('end', () => void (partial && write(`${prefix}${partial}\n`)));
}

// Runs `fn` after every earlier call for `key` in `chains` has settled.
function inTurn<T>(chains: Map<string, Promise<unknown>>, key: string, fn: () => Promise<T>): Promise<T> {
  const next = (chains.get(key) ?? Promise.resolve()).catch(() => {}).then(fn);
  chains.set(key, next);
  void next.finally(() => chains.get(key) === next && chains.delete(key)).catch(() => {});
  return next;
}

const CRASH_BUDGET = 3;
const CRASH_WINDOW_MS = 60_000;

// Shared host-driver logic: the session registry, directories and lifecycle bookkeeping; `launch` is the part that differs per host.
export function sessionHost(opts: { dataDir: string; agentsDir?: string; defaultAgent?: string; /** The rubric each agent runs with, stored beside each version so diffs cover it. */ rubricFor?: RubricFor; /** Hears every session process that exits, stopped or crashed. */ onExit?: (key: string) => void }, launch: Launch): HostDriver {
  const children = new Map<string, Child>();
  const starting = new Map<string, Promise<string>>();
  const crashes = new Map<string, number[]>();
  // Binding, stopping and removing one session run in turn, so two first requests never copy its bundle at once
  // and a delete never races a start.
  const lifecycle = new Map<string, Promise<unknown>>();

  const sessionDir = (key: string) => join(opts.dataDir, sessionDirName(key));

  // Each agent.json is read fresh, so a new session binds to the newest definition; bind() then snapshots it for that session (DEP-3).
  // Each version is copied once into the data dir (every registry run awaits that one copy), so history outlives the image's agents dir; its promotion marker lives there too.
  const kept = new Map<string, Promise<void>>();
  async function registry(): Promise<Array<AgentInfo & { path: string; retention?: unknown }>> {
    const dirs = opts.agentsDir ? await readdir(opts.agentsDir, { withFileTypes: true }).catch(() => []) : [];
    const found = await Promise.all(dirs.filter((d) => d.isDirectory()).map(async (d) => {
      const dir = join(opts.agentsDir!, d.name);
      const path = join(dir, 'agent.json');
      const def = await readJson(path).catch(() => null);
      if (typeof def?.name !== 'string' || typeof def.version !== 'number' || !SAFE_NAME.test(def.name)) return undefined;
      const id = `${def.name}@${def.version}`;
      if (!kept.has(id)) kept.set(id, keepVersion(opts.dataDir, dir, opts.rubricFor).catch(() => void kept.delete(id)));
      await kept.get(id);
      const status: AgentStatus = def.status === 'promoted' || (await exists(join(versionDir(opts.dataDir, def.name, def.version), 'promoted'))) ? 'promoted' : 'draft';
      return { name: def.name, version: def.version, status, path, retention: def.retention?.idleDays, ...(typeof def.model === 'string' && { model: def.model }), ...(typeof def.description === 'string' && { description: def.description }) };
    }));
    return found.filter((a) => a !== undefined);
  }
  void registry();

  const defaultPath = opts.defaultAgent ?? process.env.QUACK_AGENT;
  async function defaultName() {
    return defaultPath ? ((await readJson(defaultPath).catch(() => null))?.name as string | undefined) ?? '' : '';
  }

  // A version is immutable: the first session pinned to it records its content hash beside the stored version, and a source that differs is refused.
  async function assertUnforked(name: string, source: string) {
    const version = (await readJson(source).catch(() => null))?.version;
    if (typeof version !== 'number') return;
    const want = await digest(dirname(source));
    const record = join(versionDir(opts.dataDir, name, version), 'digest');
    const known = (await readText(record)) ?? (await recordDigest(record, name, version, want));
    if (known !== want) throw new AgentError(409, `agent ${name} version ${version} changed since it was first used; bump its version`);
  }

  // Versions pinned before the record existed are scanned once; `wx` keeps the first writer's hash when two sessions race.
  async function recordDigest(record: string, name: string, version: number, want: string): Promise<string> {
    let first = want;
    for (const d of await readdir(opts.dataDir).catch(() => [])) {
      const snap = join(opts.dataDir, d, SNAPSHOT);
      const def = await readJson(join(snap, 'agent.json')).catch(() => null);
      if (def?.name !== name || def.version !== version) continue;
      first = await digest(snap);
      if (first !== want) break;
    }
    await mkdir(dirname(record), { recursive: true });
    await writeFile(record, first, { flag: 'wx' }).catch(() => undefined);
    return (await readText(record)) ?? first;
  }

  // Copies the agent's bundle into the session dir; every later start loads that copy, never the agents dir (DEP-3).
  async function snapshot(name: string, source: string, dir: string, isNew: boolean) {
    if (isNew) await assertUnforked(name, source);
    // A previous role's files must not linger beside the new bundle.
    await rm(join(dir, SNAPSHOT), { recursive: true, force: true });
    await cp(dirname(source), join(dir, SNAPSHOT), { recursive: true });
  }

  async function source(name: string, fallback: string): Promise<string | undefined> {
    const found = (await registry()).find((a) => a.name === name)?.path ?? (name === fallback ? defaultPath : undefined);
    if (!found && name) throw new AgentError(404, `unknown agent ${name}`);
    return found;
  }

  // Only a submission that starts a run may change the role (see rebind); any other request must agree with the record.
  async function bind(key: string, requested?: string): Promise<{ name: string; path: string | undefined }> {
    const dir = sessionDir(key);
    const snapshotDir = join(dir, SNAPSHOT);
    const recorded = await readText(join(dir, 'agent'));
    // Where a key is born it must be one the Sessions API and the lifecycle channel can address.
    if (recorded === undefined && !SESSION_KEY.test(key)) throw new AgentError(400, 'a session key is 1 to 200 of A-Z a-z 0-9 : / # . _ @ -');
    if (recorded !== undefined && requested && requested !== recorded) throw new AgentError(409, `session runs agent ${recorded}, not ${requested}`);
    // A recorded session with no snapshot (pre-upgrade, or a lost copy) deliberately re-pins to the current definition once.
    if (recorded !== undefined && (await stat(snapshotDir).catch(() => null))) return { name: recorded, path: join(snapshotDir, 'agent.json') };
    const fallback = await defaultName();
    const name = recorded ?? requested ?? fallback;
    const from = await source(name, fallback);
    if (from) await snapshot(name, from, dir, recorded === undefined);
    if (recorded === undefined) {
      await mkdir(dir, { recursive: true });
      await writeFile(join(dir, 'session-key'), key);
      await writeFile(join(dir, 'agent'), name);
      const born = String(Date.now());
      await writeFile(join(dir, 'created'), born);
      await writeFile(join(dir, 'activity'), born);
    }
    return { name, path: from && join(snapshotDir, 'agent.json') };
  }

  const agentOf = (key: string) => readText(join(sessionDir(key), 'agent'));

  // Rewrites the snapshot and record the next start reads, so a restart resumes with the role of the latest run.
  const rebind = (key: string, name: string) => inTurn(lifecycle, key, async () => {
    const from = await source(name, await defaultName());
    if (from) await snapshot(name, from, sessionDir(key), true);
    await writeFile(join(sessionDir(key), 'agent'), name);
  });

  async function start(key: string, agentPath: string | undefined): Promise<string> {
    const launched = await launch({ key, dir: sessionDir(key), agentPath });
    const child: Child = { launched, stopping: false };
    // A crash after startup restarts from the log, up to a budget so a process-level crash cannot loop;
    // past it the next request restarts the session. Startup crashes are reported to the caller instead.
    launched.onExit(() => {
      opts.onExit?.(key);
      if (children.get(key) !== child) return;
      children.delete(key);
      if (child.stopping) return;
      const recent = (crashes.get(key) ?? []).filter((t) => Date.now() - t < CRASH_WINDOW_MS).concat(Date.now());
      crashes.set(key, recent);
      if (recent.length <= CRASH_BUDGET) void ensure(key).catch(() => {});
    });
    children.set(key, child);
    return launched.url;
  }

  // The start itself runs outside the turn (wrapped, so the turn does not wait on it), and every caller shares it, failure included.
  async function ensure(key: string, agent?: string): Promise<string> {
    const { url } = await inTurn(lifecycle, key, async () => {
      const { path } = await bind(key, agent);
      const live = children.get(key);
      if (live) return { url: Promise.resolve(live.launched.url) };
      let pending = starting.get(key);
      if (!pending) {
        pending = start(key, path).finally(() => starting.delete(key));
        starting.set(key, pending);
      }
      return { url: pending };
    });
    return url;
  }

  const agents = async () => (await registry()).map(({ name, version, status, model, description }) => ({ name, version, status, ...(model && { model }), ...(description && { description }) }));

  // Stored versions come first (they carry the rubric); a version only a session pinned fills in beneath.
  async function agent(name: string, version?: number): Promise<AgentView | undefined> {
    if (!SAFE_NAME.test(name)) return undefined;
    const current = (await registry()).find((a) => a.name === name);
    const stored = await readdir(join(opts.dataDir, STORE, name)).catch(() => []);
    const pinned = (await readdir(opts.dataDir).catch(() => [])).filter((d) => d !== STORE).map((d) => join(opts.dataDir, d, SNAPSHOT));
    const dirs = [...stored.map((v) => join(opts.dataDir, STORE, name, v)), ...pinned];
    const held = (await Promise.all(dirs.map((d) => readBundle(d).catch(() => undefined)))).filter((b) => b?.name === name) as AgentDetail[];
    const versions = [...new Map(held.reverse().map((b) => [b.version, b])).values()].sort((a, b) => b.version - a.version);
    const want = versions.find((b) => b.version === (version ?? current?.version ?? versions[0]?.version));
    return want && { ...want, versions: versions.map((b) => ({ version: b.version, status: b.status })) };
  }

  // The image's agents dir is replaced on deploy, so promotion is a marker in the version store, which the registry overlays.
  async function promote(name: string, version: number): Promise<AgentStatus> {
    const current = (await registry()).find((a) => a.name === name);
    if (!current) throw new AgentError(404, `unknown agent ${name}`);
    if (current.version !== version) throw new AgentError(409, `agent ${name} is now at version ${current.version}`);
    await writeFile(join(versionDir(opts.dataDir, name, version), 'promoted'), '');
    return 'promoted';
  }

  // Retention is storage policy, so a session pinned before it existed follows its agent's current bundle.
  async function pinned(key: string) {
    const def = await readJson(join(sessionDir(key), SNAPSHOT, 'agent.json')).catch(() => null);
    const agent = await agentOf(key);
    const idleDays = def?.retention?.idleDays ?? (await registry()).find((a) => a.name === agent)?.retention;
    return { ...(typeof def?.version === 'number' && { version: def.version }), ...(typeof idleDays === 'number' && { retention: { idleDays } }) };
  }

  // Touches of one session run in turn, so an older read can never be written over a newer one.
  const touching = new Map<string, Promise<unknown>>();
  const touch = (key: string, at: number) => inTurn(touching, key, async () => writeFile(join(sessionDir(key), 'activity'), String(Math.max(at, await lastActivity(sessionDir(key))))));

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

  // A process still running or starting for the session is stopped first, so none is left bound to a deleted directory.
  const remove = (key: string) => inTurn(lifecycle, key, async () => {
    const dir = sessionDir(key);
    if ((await readText(join(dir, 'session-key'))) === undefined) return false;
    await stopNow(key);
    await rm(dir, { recursive: true, force: true });
    return true;
  });

  // A start in flight is waited out, then stopped, so a stop never leaves a process that comes up afterwards.
  async function stopNow(key: string) {
    await starting.get(key)?.catch(() => {});
    const child = children.get(key);
    if (!child) return;
    child.stopping = true;
    children.delete(key);
    await child.launched.stop();
  }
  const stop = (key: string) => inTurn(lifecycle, key, () => stopNow(key));

  return { ensure, agentOf, rebind, agents, agent, promote, pinned, exported: (key) => readExport(sessionDir(key)), stop, remove, setWake, touch, wakes, running: () => [...children.keys()], pid: (key) => children.get(key)?.launched.pid, credential: (key) => children.get(key)?.launched.token };
}

/** Development host driver: one child process per session on this machine. Container drivers replace it in deployment. */
export function processDriver(opts: LauncherOptions & { dataDir: string; agentsDir?: string; defaultAgent?: string; rubricFor?: RubricFor; onExit?: (key: string) => void }): HostDriver {
  return sessionHost(opts, processLauncher(opts));
}

type LauncherOptions = { command: string[]; /** Variables from the host a session process receives, by its agent's definition path; default: toolchain basics only. */ inherit?: (agentPath: string | undefined) => NodeJS.ProcessEnv | Promise<NodeJS.ProcessEnv>; env?: NodeJS.ProcessEnv; /** Mints each launch's QUACK_INDEX_TOKEN and revokes it when the process exits. */ broker?: Pick<CredentialBroker, 'mint' | 'revoke'>; startTimeoutMs?: number; /** How long a stopped process gets to exit on SIGTERM before SIGKILL. */ stopTimeoutMs?: number; /** Test seam for the stop deadline. */ setTimer?: SetTimer };

// Below the distributor's 20 s drain, so a drain still finishes after a kill.
const STOP_TIMEOUT_MS = 10_000;

function processLauncher(opts: LauncherOptions): Launch {
  return async ({ key, dir, agentPath }) => {
    const [cmd, ...args] = opts.command;
    // The session process gets an allowlist, never the host's whole environment; its shells narrow it further.
    const inherited = await (opts.inherit?.(agentPath) ?? shellEnv(process.env));
    const serverToken = await writeLaunchToken(dir);
    const token = opts.broker?.mint(key);
    const env: NodeJS.ProcessEnv = { ...inherited, ...opts.env, ...(token && { QUACK_INDEX_TOKEN: token }), ...(agentPath && { QUACK_AGENT: agentPath }), PORT: '0', QUACK_SESSION_HOST: '127.0.0.1', QUACK_SESSION_TOKEN_FILE: join(dir, LAUNCH_TOKEN_FILE), QUACK_SESSION_DIR: dir, QUACK_SESSION_KEY: key };
    const proc = spawn(cmd, args, { env, stdio: ['ignore', 'pipe', 'pipe', 'ipc'] });
    if (token) proc.once('exit', () => opts.broker!.revoke(token));
    // Recorded so a later distributor finds and stops it if this one dies first.
    if (proc.pid !== undefined) await recordProcess(dir, proc.pid).catch(() => {});
    proc.once('exit', () => void forgetProcess(dir).catch(() => {}));
    // Output reaches this process's streams, line by line under the session's key; the tail only rides along in a startup failure.
    let tail = '';
    const keep = (b: Buffer) => void (tail = (tail + b).slice(-OUTPUT_TAIL));
    forwardOutput(proc.stdout!, process.stdout, keep, `[${key}] `);
    forwardOutput(proc.stderr!, process.stderr, keep, `[${key}] `);
    try {
      // The session binds its own port and reports it, so the parent never probes a port another process may own.
      const url = `http://127.0.0.1:${await childPort(proc, 'session process', opts.startTimeoutMs ?? 15_000)}`;
      return {
        url,
        token: serverToken,
        pid: proc.pid,
        onExit: (cb) => (proc.exitCode !== null || proc.signalCode ? cb() : void proc.once('exit', cb)),
        stop: async () => {
          if (proc.exitCode !== null || proc.signalCode) return;
          const exited = new Promise((r) => proc.once('exit', r));
          proc.kill('SIGTERM');
          // A process that ignores SIGTERM or never finishes closing must not wedge every request for its key.
          const cancel = (opts.setTimer ?? unrefTimer)(() => proc.kill('SIGKILL'), opts.stopTimeoutMs ?? STOP_TIMEOUT_MS);
          await exited;
          cancel();
        },
      };
    } catch (err) {
      proc.kill('SIGKILL');
      throw new Error(`${(err as Error).message}; output: ${tail.trim() || '(none)'}`);
    }
  };
}

import { spawn, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { join } from 'node:path';

export type HostDriver = {
  /** Starts the session's process if it is not running and returns its base URL. */
  ensure(key: string): Promise<string>;
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

// Development host driver: one child process per session on this machine (A11). Container drivers replace it in deployment.
export function processDriver(opts: { command: string[]; dataDir: string; env?: NodeJS.ProcessEnv; startTimeoutMs?: number }): HostDriver {
  const children = new Map<string, Child>();
  const starting = new Map<string, Promise<string>>();

  async function start(key: string): Promise<string> {
    const dir = join(opts.dataDir, sessionDirName(key));
    await mkdir(dir, { recursive: true });
    await writeFile(join(dir, 'session-key'), key);
    const port = await freePort();
    const url = `http://127.0.0.1:${port}`;
    const [cmd, ...args] = opts.command;
    const proc = spawn(cmd, args, {
      env: { ...process.env, ...opts.env, PORT: String(port), QUACK_SESSION_DIR: dir, QUACK_SESSION_KEY: key },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    const child: Child = { proc, url, stopping: false };
    // A crash after startup restarts the session from its log right away (NFR-HAR-2); pi-durable parks work that keeps
    // crashing. A crash during startup is reported to the caller instead, so a broken config cannot restart-loop.
    proc.once('exit', () => {
      if (children.get(key) !== child) return;
      children.delete(key);
      if (!child.stopping) void ensure(key).catch(() => {});
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

  function ensure(key: string): Promise<string> {
    const live = children.get(key);
    if (live) return Promise.resolve(live.url);
    let pending = starting.get(key);
    if (!pending) {
      pending = start(key).finally(() => starting.delete(key));
      starting.set(key, pending);
    }
    return pending;
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

  return { ensure, stop, running: () => [...children.keys()], pid: (key) => children.get(key)?.proc.pid };
}

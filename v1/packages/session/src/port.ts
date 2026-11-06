import type { ChildProcess } from 'node:child_process';
import { readFile, rm } from 'node:fs/promises';
import type { AddressInfo, Server } from 'node:net';

/** Listens, then tells a parent holding an IPC channel the bound port; a port the parent picked and released can be taken by another process first. */
export function listenReporting<S extends Server>(server: S, port = 0, host = '127.0.0.1'): S {
  return server.listen(port, host, () => process.send?.({ port: (server.address() as AddressInfo).port }));
}

/** The port a child reports from listenReporting; rejects with `${name} exited with N` if it dies first, or when it stays silent past the timeout. */
export function childPort(child: ChildProcess, name: string, timeoutMs: number): Promise<number> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => done(() => reject(Object.assign(new Error(`${name} did not report a port`), { code: 'E_NO_PORT' }))), timeoutMs);
    const onMessage = (m: unknown) => typeof (m as { port?: unknown })?.port === 'number' && done(() => resolve((m as { port: number }).port));
    const onExit = () => done(() => reject(new Error(`${name} exited with ${child.exitCode ?? child.signalCode}`)));
    const done = (settle: () => void) => {
      clearTimeout(timer);
      child.off('message', onMessage).off('exit', onExit);
      settle();
    };
    child.on('message', onMessage).once('exit', onExit);
    if (child.exitCode !== null || child.signalCode) onExit();
  });
}

/** The launch's server token, from the file its host wrote (QUACK_SESSION_TOKEN_FILE); removed once read, as the session's later shells run as this user. */
// Not an environment variable: those stay readable to such a shell in /proc/<pid>/environ.
export async function takeLaunchToken(env: NodeJS.ProcessEnv = process.env): Promise<string> {
  const file = env.QUACK_SESSION_TOKEN_FILE;
  if (!file) throw new Error('QUACK_SESSION_TOKEN_FILE is required');
  const token = (await readFile(file, 'utf8')).trim();
  await rm(file, { force: true });
  if (!token) throw new Error(`${file} holds no token`);
  return token;
}

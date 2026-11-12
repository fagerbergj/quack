import type { ChildProcess } from 'node:child_process';
import { readFile, rm } from 'node:fs/promises';
import type { AddressInfo, Server } from 'node:net';
import type { SetTimer } from './lifecycle-sender.ts';

/** Listens, then tells a parent holding an IPC channel the bound port; a port the parent picked and released can be taken by another process first. */
export function listenReporting<S extends Server>(server: S, port = 0, host = '127.0.0.1'): S {
  return server.listen(port, host, () => process.send?.({ port: (server.address() as AddressInfo).port }));
}

/** The port a child reports from listenReporting; rejects if it cannot be spawned, dies first, or stays silent past the timeout. */
export function childPort(child: ChildProcess, name: string, timeoutMs: number, setTimer: SetTimer = refTimer): Promise<number> {
  return new Promise((resolve, reject) => {
    const cancel = setTimer(() => done(() => reject(Object.assign(new Error(`${name} did not report a port within ${timeoutMs} ms; pid ${child.pid} is still running`), { code: 'E_NO_PORT' }))), timeoutMs);
    const onMessage = (m: unknown) => typeof (m as { port?: unknown })?.port === 'number' && done(() => resolve((m as { port: number }).port));
    const onExit = () => done(() => reject(new Error(`${name} exited with ${child.exitCode ?? child.signalCode}`)));
    // A failed spawn emits 'error' and never 'exit'; unheard, it would also throw in the parent.
    const onError = (err: Error) => done(() => reject(new Error(`${name} failed to start: ${err.message}`)));
    const done = (settle: () => void) => {
      cancel();
      child.off('message', onMessage).off('exit', onExit).off('error', onError);
      settle();
    };
    child.on('message', onMessage).once('exit', onExit).once('error', onError);
    if (child.exitCode !== null || child.signalCode) onExit();
  });
}

const refTimer: SetTimer = (fn, ms) => {
  const t = setTimeout(fn, ms);
  return () => clearTimeout(t);
};

/** The launch's server token, from the file its host wrote (QUACK_SESSION_TOKEN_FILE); removed once read, as the session's later shells run as this user. */
// Not an environment variable: those stay readable to such a shell in /proc/<pid>/environ.
export async function takeLaunchToken(env: NodeJS.ProcessEnv = process.env): Promise<string> {
  const file = env.QUACK_SESSION_TOKEN_FILE;
  if (!file) throw new Error('QUACK_SESSION_TOKEN_FILE is required');
  return takeToken(file);
}

async function takeToken(file: string): Promise<string> {
  const token = (await readFile(file, 'utf8')).trim();
  await rm(file, { force: true });
  if (!token) throw new Error(`${file} holds no token`);
  return token;
}

/** The variables that carry the session listener and this launch's outbound credential, and the pair they replaced; the runtime drops them all once read. */
export const LAUNCH_ENV = ['QUACK_SESSION_LISTENER_URL', 'QUACK_LAUNCH_TOKEN_FILE', 'QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN'] as const;

/** The session listener and the outbound credential the distributor handed this launch, the credential file removed once read; undefined when it handed neither. */
export async function takeLaunchEndpoint(env: NodeJS.ProcessEnv = process.env): Promise<{ url: string; token: string } | undefined> {
  const { QUACK_SESSION_LISTENER_URL: url, QUACK_LAUNCH_TOKEN_FILE: file } = env;
  if (url && file) return { url, token: await takeToken(file) };
  if (url || file) throw new Error('QUACK_SESSION_LISTENER_URL and QUACK_LAUNCH_TOKEN_FILE are set together or not at all');
  // shortcut: the pre-rename pair, read for one release as the env contract allows; drop it in the release after.
  return env.QUACK_INDEX_URL && env.QUACK_INDEX_TOKEN ? { url: env.QUACK_INDEX_URL, token: env.QUACK_INDEX_TOKEN } : undefined;
}

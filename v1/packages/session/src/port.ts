import type { ChildProcess } from 'node:child_process';
import type { AddressInfo, Server } from 'node:net';

/** Listens, then tells a parent holding an IPC channel the bound port; a port the parent picked and released can be taken by another process first. */
export function listenReporting<S extends Server>(server: S, port = 0): S {
  return server.listen(port, () => process.send?.({ port: (server.address() as AddressInfo).port }));
}

/** The port a child reports from listenReporting; rejects with `${name} exited with N` if it dies first, or when it stays silent past the timeout. */
export function childPort(child: ChildProcess, name: string, timeoutMs: number): Promise<number> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => done(() => reject(new Error(`${name} did not report a port`))), timeoutMs);
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

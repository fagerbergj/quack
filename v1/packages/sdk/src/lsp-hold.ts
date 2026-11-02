// Stdio proxy `node lsp-hold.ts <server> [args]`: pi-lsp drops $/progress, so this holds references until the first load ends.
// A server that never reports progress stalls them until pi-lsp's request timeout.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

type Message = { method?: string; params?: { token?: unknown; value?: { kind?: string } } };

/** Splits an LSP byte stream into whole frames (header and body) and their parsed bodies. */
export function frames(onFrame: (raw: Buffer, message: Message) => void): (chunk: Buffer) => void {
  let buf = Buffer.alloc(0);
  return (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    for (;;) {
      const end = buf.indexOf('\r\n\r\n');
      if (end < 0) return;
      const length = Number(/Content-Length: (\d+)/i.exec(buf.subarray(0, end).toString())?.[1]);
      if (!Number.isFinite(length)) throw new Error('LSP frame without Content-Length');
      if (buf.length < end + 4 + length) return;
      const raw = buf.subarray(0, end + 4 + length);
      buf = buf.subarray(raw.length);
      onFrame(raw, JSON.parse(raw.subarray(end + 4).toString()));
    }
  };
}

/** The load state of one proxied server: references wait until its first progress cycle has opened and every open token has ended. */
export function holder(toServer: (raw: Buffer) => void) {
  const loading = new Set<unknown>();
  let loaded = false;
  const held: Buffer[] = [];
  return {
    fromClient: (raw: Buffer, m: Message) => (m.method === 'textDocument/references' && !loaded ? held.push(raw) : toServer(raw)),
    fromServer: (m: Message) => {
      if (m.method === 'window/workDoneProgress/create') loading.add(m.params?.token);
      const ended = m.method === '$/progress' && m.params?.value?.kind === 'end';
      if (ended) loading.delete(m.params?.token);
      loaded ||= ended && !loading.size;
      if (loaded) held.splice(0).forEach(toServer);
    },
  };
}

/** The pi-lsp server entry rewritten to run through the proxy, so a reference query waits for the project to load. */
export function holdUntilLoaded<S extends { bin: string; args?: string[] }>(server: S): S {
  return { ...server, bin: process.execPath, args: [fileURLToPath(import.meta.url), server.bin, ...(server.args ?? [])] };
}

if (import.meta.main) {
  const [bin, ...args] = process.argv.slice(2);
  const child = spawn(bin, args, { stdio: ['pipe', 'pipe', 'inherit'] });
  const hold = holder((raw) => child.stdin.write(raw));
  child.stdout.on('data', frames((raw, m) => { process.stdout.write(raw); hold.fromServer(m); }));
  process.stdin.on('data', frames(hold.fromClient));
  process.stdin.on('end', () => child.stdin.end());
  child.on('exit', (code) => process.exit(code ?? 1));
  process.on('SIGTERM', () => { child.kill('SIGTERM'); process.exit(0); });
}

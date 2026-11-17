// Preloaded into every node:test file process: a file whose handles keep it alive after its tests fails, naming them.
// It replaces --test-force-exit, which ended such a process silently and hid the leak.
import { ChildProcess } from 'node:child_process';
import { after } from 'node:test';
import { relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// The file's own root after-hooks run after this one, so the grace covers them; cleanup slower than this is itself a leak.
const GRACE_MS = Number(process.env.QUACK_LEAK_GRACE_MS ?? 10_000);
const ROOT = fileURLToPath(new URL('..', import.meta.url));

/** Known leaks to retire, as test file: what stays open. A listed file exits quietly; one that stops leaking fails. */
export const KNOWN: Record<string, string> = {};

/** The test file a process runs, relative to the repo root. */
export const fileOf = (argv: string[]) => relative(ROOT, resolve(argv.findLast((a) => /\.test\.tsx?$/.test(a)) ?? ''));

export function leakReport(file: string, resources: string[], known: Record<string, string> = KNOWN): { code: number; message: string } {
  if (file in known) return { code: 0, message: `${file}: known leak (${known[file]}): ${resources.join(', ')}` };
  return { code: 1, message: `${file}: still alive ${GRACE_MS / 1000}s after its tests, held by ${resources.join(', ')}; close them in an after hook` };
}

if (process.env.NODE_TEST_CONTEXT) {
  const file = fileOf(process.argv);
  let leaked = false;
  after(() => {
    setTimeout(() => {
      leaked = true;
      const { code, message } = leakReport(file, process.getActiveResourcesInfo());
      // A child left running holds this file's output pipes, and the runner reads them to EOF: it would wait forever.
      // shortcut: kills direct children only; a grandchild holding the pipes still stalls the run until the CI job timeout.
      const children = (process as unknown as { _getActiveHandles(): unknown[] })._getActiveHandles().filter((h) => h instanceof ChildProcess);
      for (const c of children) c.kill('SIGKILL');
      process.stderr.write(`${message}${children.map((c) => `; killed child ${c.pid} (${c.spawnargs.join(' ')})`).join('')}\n`);
      process.exit(code);
    }, GRACE_MS).unref();
  });
  process.on('exit', (code) => {
    if (leaked || code !== 0 || !(file in KNOWN)) return;
    process.stderr.write(`${file} no longer leaks; remove it from KNOWN in tools/leak-check.ts\n`);
    process.exitCode = 1;
  });
}

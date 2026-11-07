// Session processes a previous distributor left running: found by the record each launch writes, and stopped before
// anything else touches their logs, as the docker driver's reap does for containers.
import { readFileSync } from 'node:fs';
import { mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { basename, dirname, join } from 'node:path';

/** The distributor's own directory of launch records, beside the session dirs and never inside one. */
export const RECORDS = '.session-processes';

/** The few process-table facts the reaper needs; the real one reads /proc, a test passes its own. */
export type ProcessTable = {
  /** A process's start time, stable for its life, so a reused pid never matches; undefined when no such process. */
  startOf(pid: number): string | undefined;
  /** Its argv, and its environment's QUACK_SESSION_DIR; undefined when unreadable. */
  identity(pid: number): { argv: string[]; sessionDir?: string } | undefined;
  kill(pid: number, signal: NodeJS.Signals): void;
  /** Resolves once the process is gone or the time is up, whichever is first. */
  gone(pid: number, ms: number): Promise<boolean>;
};

const read = (path: string) => { try { return readFileSync(path, 'utf8'); } catch { return undefined; } };

// /proc/<pid>/stat's field 22 is the start time in clock ticks since boot; the command name may hold spaces, so fields count after ')'.
function procStart(pid: number): string | undefined {
  const stat = read(`/proc/${pid}/stat`);
  return stat?.slice(stat.lastIndexOf(')') + 2).split(' ')[19];
}

/** The host's process table, from /proc. */
export const procTable: ProcessTable = {
  startOf: procStart,
  identity(pid) {
    const argv = read(`/proc/${pid}/cmdline`)?.split('\0').filter(Boolean);
    const env = read(`/proc/${pid}/environ`)?.split('\0').find((e) => e.startsWith('QUACK_SESSION_DIR='));
    return argv && { argv, ...(env && { sessionDir: env.slice('QUACK_SESSION_DIR='.length) }) };
  },
  kill: (pid, signal) => { try { process.kill(pid, signal); } catch { /* already gone */ } },
  async gone(pid, ms) {
    const start = procStart(pid);
    for (let waited = 0; waited < ms && start !== undefined && procStart(pid) === start; waited += 50) await new Promise((r) => setTimeout(r, 50));
    return procStart(pid) !== start || start === undefined;
  },
};

const recordOf = (sessionDir: string) => join(dirname(sessionDir), RECORDS, `${basename(sessionDir)}.json`);

/** Records a launched process in the distributor's area, keyed by its session's directory name. */
export async function recordProcess(sessionDir: string, pid: number, table: ProcessTable = procTable) {
  const start = table.startOf(pid);
  if (start === undefined) return;
  await mkdir(dirname(recordOf(sessionDir)), { recursive: true });
  await writeFile(recordOf(sessionDir), JSON.stringify({ pid, start }));
}

export const forgetProcess = (sessionDir: string) => rm(recordOf(sessionDir), { force: true });

// The recorded process only: the same start time (no reused pid), the session runtime's entrypoint, and that session's directory.
function isOurs(table: ProcessTable, rec: { pid: number; start: string }, entry: string, sessionDir: string): boolean {
  if (table.startOf(rec.pid) !== rec.start) return false;
  const id = table.identity(rec.pid);
  return !!id && id.argv.includes(entry) && id.sessionDir === sessionDir;
}

/** Stops every session process a previous run left (TERM, then KILL after `graceMs`); returns the session dirs it reaped. */
export async function reapOrphans(dataDir: string, opts: { entry: string; table?: ProcessTable; graceMs?: number; log?: (m: string) => void }): Promise<string[]> {
  const table = opts.table ?? procTable;
  const log = opts.log ?? ((m: string) => console.warn(m));
  const reaped: string[] = [];
  for (const file of await readdir(join(dataDir, RECORDS)).catch(() => [] as string[])) {
    const path = join(dataDir, RECORDS, file);
    const rec = await readFile(path, 'utf8').then((t) => JSON.parse(t) as { pid: number; start: string }, () => undefined);
    const name = file.replace(/\.json$/, '');
    if (rec && isOurs(table, rec, opts.entry, join(dataDir, name))) {
      table.kill(rec.pid, 'SIGTERM');
      if (!(await table.gone(rec.pid, opts.graceMs ?? 10_000))) table.kill(rec.pid, 'SIGKILL');
      reaped.push(name);
    } else if (rec && table.startOf(rec.pid) !== undefined) log(`session record ${name} names pid ${rec.pid}, which is not that session's runtime; left alone`);
    await rm(path, { force: true });
  }
  return reaped;
}

// Session processes a previous distributor left running: found by the pid file each launch writes, and stopped before
// anything else touches their logs, as the docker driver's reap does for containers.
import { readFileSync } from 'node:fs';
import { readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

/** Where in its directory a session's process records its pid and start time while it runs. */
export const PID_FILE = '.process';

/** The few process-table facts the reaper needs; the real one reads /proc, a test passes its own. */
export type ProcessTable = {
  /** A process's start time, stable for its life, so a reused pid never matches; undefined when no such process. */
  startOf(pid: number): string | undefined;
  kill(pid: number, signal: NodeJS.Signals): void;
  /** Resolves once the process is gone or the time is up, whichever is first. */
  gone(pid: number, ms: number): Promise<boolean>;
};

// /proc/<pid>/stat's field 22 is the start time in clock ticks since boot; the command name may hold spaces, so fields count after ')'.
function procStart(pid: number): string | undefined {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
    return stat.slice(stat.lastIndexOf(')') + 2).split(' ')[19];
  } catch {
    return undefined;
  }
}

/** The host's process table, from /proc. */
export const procTable: ProcessTable = {
  startOf: procStart,
  kill: (pid, signal) => { try { process.kill(pid, signal); } catch { /* already gone */ } },
  async gone(pid, ms) {
    const start = procStart(pid);
    for (let waited = 0; waited < ms && start !== undefined && procStart(pid) === start; waited += 50) await new Promise((r) => setTimeout(r, 50));
    return procStart(pid) !== start || start === undefined;
  },
};

/** Records a launched process in its session's directory. */
export async function recordProcess(dir: string, pid: number, table: ProcessTable = procTable) {
  const start = table.startOf(pid);
  if (start !== undefined) await writeFile(join(dir, PID_FILE), JSON.stringify({ pid, start }));
}

export const forgetProcess = (dir: string) => rm(join(dir, PID_FILE), { force: true });

/** Stops every session process a previous run left behind (TERM, then KILL after `graceMs`); returns the dirs it reaped. */
export async function reapOrphans(dataDir: string, opts: { table?: ProcessTable; graceMs?: number } = {}): Promise<string[]> {
  const table = opts.table ?? procTable;
  const reaped: string[] = [];
  for (const d of await readdir(dataDir).catch(() => [] as string[])) {
    const file = join(dataDir, d, PID_FILE);
    const found = await readFile(file, 'utf8').then((t) => JSON.parse(t) as { pid: number; start: string }, () => undefined);
    if (!found) continue;
    // Only the very process recorded: a pid since reused by another process has another start time.
    if (table.startOf(found.pid) === found.start) {
      table.kill(found.pid, 'SIGTERM');
      if (!(await table.gone(found.pid, opts.graceMs ?? 10_000))) table.kill(found.pid, 'SIGKILL');
      reaped.push(d);
    }
    await rm(file, { force: true });
  }
  return reaped;
}

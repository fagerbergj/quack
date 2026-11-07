import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createDistributor, processDriver, sessionDirName } from '../src/index.ts';
import { RECORDS, reapOrphans, recordProcess, type ProcessTable } from '../src/orphans.ts';

const dirs: string[] = [];
after(async () => { for (const d of dirs) await rm(d, { recursive: true, force: true }); });
const temp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-orphans-')); dirs.push(d); return d; };

const ENTRY = '/app/packages/runtime/src/main.ts';
type Proc = { start: string; argv?: string[]; dir?: string };

// A process table in memory: processes by pid with start time, argv and session dir, which ignore TERM, and every signal sent.
function fakeTable(procs: Record<number, Proc>, stubborn: number[] = []) {
  const live = new Map(Object.entries(procs).map(([pid, p]) => [Number(pid), p]));
  const signals: string[] = [];
  const table: ProcessTable = {
    startOf: (pid) => live.get(pid)?.start,
    identity: (pid) => { const p = live.get(pid); return p && { argv: p.argv ?? [], ...(p.dir && { sessionDir: p.dir }) }; },
    kill: (pid, signal) => { signals.push(`${pid} ${signal}`); if (signal === 'SIGKILL' || !stubborn.includes(pid)) live.delete(pid); },
    gone: async (pid) => !live.has(pid),
  };
  return { table, signals, live };
}

async function record(dataDir: string, name: string, rec: { pid: number; start: string }) {
  await mkdir(join(dataDir, RECORDS), { recursive: true });
  await writeFile(join(dataDir, RECORDS, `${name}.json`), JSON.stringify(rec));
}
const runtime = (dataDir: string, name: string, start: string): Proc => ({ start, argv: ['node', ENTRY], dir: join(dataDir, name) });
const quiet = { log: () => {} };

test('a session runtime a previous run left is stopped before ready, and a stubborn one is killed', async () => {
  const dataDir = await temp();
  await record(dataDir, 'a', { pid: 101, start: 's101' });
  await record(dataDir, 'b', { pid: 102, start: 's102' });
  const { table, signals, live } = fakeTable({ 101: runtime(dataDir, 'a', 's101'), 102: runtime(dataDir, 'b', 's102'), 999: { start: 'other' } }, [102]);
  assert.deepEqual((await reapOrphans(dataDir, { entry: ENTRY, table, ...quiet })).sort(), ['a', 'b']);
  assert.deepEqual(signals, ['101 SIGTERM', '102 SIGTERM', '102 SIGKILL']);
  assert.deepEqual([...live.keys()], [999], 'nothing else is touched');
  assert.deepEqual(await readdir(join(dataDir, RECORDS)), [], 'the records are cleared');
});

test('a record for a process that is not a session runtime, or names another session, is never signalled', async () => {
  const dataDir = await temp();
  await record(dataDir, 'planted', { pid: 201, start: 's201' });
  await record(dataDir, 'other-session', { pid: 202, start: 's202' });
  await record(dataDir, 'reused', { pid: 203, start: 'old-start' });
  await record(dataDir, 'gone', { pid: 204, start: 's204' });
  const logged: string[] = [];
  const { table, signals } = fakeTable({ 201: { start: 's201', argv: ['/usr/bin/vim'], dir: join(dataDir, 'planted') }, 202: runtime(dataDir, 'somewhere-else', 's202'), 203: runtime(dataDir, 'reused', 'new-start') });
  assert.deepEqual(await reapOrphans(dataDir, { entry: ENTRY, table, log: (m) => void logged.push(m) }), []);
  assert.deepEqual(signals, []);
  assert.equal(logged.length, 3, 'each live mismatch is logged');
});

test('a launch records its process in the distributor area, never in the session directory, and the next run finds it', async () => {
  const dataDir = await temp();
  const dir = join(dataDir, 'sess');
  await mkdir(dir);
  await recordProcess(dir, 301, fakeTable({ 301: runtime(dataDir, 'sess', 's301') }).table);
  assert.deepEqual(await readdir(dir), []);
  const { table, signals } = fakeTable({ 301: runtime(dataDir, 'sess', 's301') });
  assert.deepEqual(await reapOrphans(dataDir, { entry: ENTRY, table, ...quiet }), ['sess']);
  assert.deepEqual(signals, ['301 SIGTERM']);
});

test('a key outside the session-key pattern is refused where it would be born, and nothing is created', async () => {
  const dataDir = await temp();
  const driver = processDriver({ command: [], dataDir });
  for (const key of ['a b', 'a+b', 'x'.repeat(201), '']) {
    await assert.rejects(driver.ensure(key, 'quack'), (err: Error & { status?: number }) => err.status === 400, JSON.stringify(key));
    assert.equal(existsSync(join(dataDir, sessionDirName(key))), false);
  }
  const server = createDistributor(driver, { setTimer: () => () => {} }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const port = (server.address() as { port: number }).port;
  const res = await fetch(`http://127.0.0.1:${port}/sessions/a%20b/invocations`, { method: 'POST', body: '{"input":"hi"}' });
  assert.equal(res.status, 400);
  await new Promise((r) => server.close(r));
  assert.deepEqual(await readdir(dataDir), []);
});

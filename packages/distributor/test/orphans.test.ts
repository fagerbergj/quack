import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createDistributor, processDriver, sessionDirName } from '../src/index.ts';
import { PID_FILE, reapOrphans, recordProcess, type ProcessTable } from '../src/orphans.ts';

const dirs: string[] = [];
after(async () => { for (const d of dirs) await rm(d, { recursive: true, force: true }); });
const temp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-orphans-')); dirs.push(d); return d; };

// A process table in memory: processes by pid with their start time, which ones ignore TERM, and every signal sent.
function fakeTable(procs: Record<number, string>, stubborn: number[] = []) {
  const live = new Map(Object.entries(procs).map(([pid, start]) => [Number(pid), start]));
  const signals: string[] = [];
  const table: ProcessTable = {
    startOf: (pid) => live.get(pid),
    kill: (pid, signal) => { signals.push(`${pid} ${signal}`); if (signal === 'SIGKILL' || !stubborn.includes(pid)) live.delete(pid); },
    gone: async (pid) => !live.has(pid),
  };
  return { table, signals, live };
}

async function sessionDir(dataDir: string, name: string, record?: { pid: number; start: string }) {
  const dir = join(dataDir, name);
  await mkdir(dir, { recursive: true });
  if (record) await writeFile(join(dir, PID_FILE), JSON.stringify(record));
  return dir;
}

test('a process a previous run left is stopped before ready, and a stubborn one is killed', async () => {
  const dataDir = await temp();
  await sessionDir(dataDir, 'a', { pid: 101, start: 's101' });
  await sessionDir(dataDir, 'b', { pid: 102, start: 's102' });
  await sessionDir(dataDir, 'c');
  const { table, signals, live } = fakeTable({ 101: 's101', 102: 's102', 999: 'other' }, [102]);
  assert.deepEqual((await reapOrphans(dataDir, { table })).sort(), ['a', 'b']);
  assert.deepEqual(signals, ['101 SIGTERM', '102 SIGTERM', '102 SIGKILL']);
  assert.deepEqual([...live.keys()], [999], 'nothing else is touched');
  for (const d of ['a', 'b']) assert.equal(existsSync(join(dataDir, d, PID_FILE)), false);
});

test('a pid since reused by another process, or one already gone, is left alone and its record cleared', async () => {
  const dataDir = await temp();
  await sessionDir(dataDir, 'reused', { pid: 201, start: 'old-start' });
  await sessionDir(dataDir, 'gone', { pid: 202, start: 's202' });
  const { table, signals } = fakeTable({ 201: 'new-start' });
  assert.deepEqual(await reapOrphans(dataDir, { table }), []);
  assert.deepEqual(signals, []);
  assert.deepEqual(await readdir(join(dataDir, 'reused')), []);
});

test('a launch records its process so the next run can find it', async () => {
  const dir = await temp();
  const { table } = fakeTable({ 301: 's301' });
  await recordProcess(dir, 301, table);
  const { table: next, signals } = fakeTable({ 301: 's301' });
  assert.deepEqual(await reapOrphans(join(dir, '..'), { table: next }).then((r) => r.includes(dir.split('/').at(-1)!)), true);
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

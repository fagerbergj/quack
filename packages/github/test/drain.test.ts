import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createDistributor, processDriver } from '../../distributor/src/index.ts';
import { noticeFor, settled } from '../src/notice.ts';

const cleanup: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanup) await c(); });

const COMMAND = [process.execPath, fileURLToPath(new URL('../../distributor/test/fixtures/gate-session.ts', import.meta.url))];

async function distributor(dataDir: string) {
  const driver = processDriver({ command: COMMAND, dataDir, defaultAgent: '' });
  const server = createDistributor(driver, { idleMs: 60_000, setTimer: () => () => {} }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanup.push(async () => { await server.drain(); await new Promise((r) => server.close(r)); });
  return { server, driver, base: `http://127.0.0.1:${(server.address() as AddressInfo).port}` };
}

test('a deploy drain mid-run does not read as a failed review, and the resumed run still lands', async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-drain-'));
  cleanup.push(() => rm(dataDir, { recursive: true, force: true }));
  const key = encodeURIComponent('github:a/b#9');
  const first = await distributor(dataDir);
  const url = `${first.base}/sessions/${key}/invocations`;
  const body = { input: 'review', requestId: 'github-delivery:drain' };
  await fetch(url, { method: 'POST', body: JSON.stringify(body) });
  // The gated tool is waiting on a person, so the run is mid-flight when the deploy drains.
  const waiting = settled(url, { ...body, wait: true }).then((outcome) => ({ outcome }), (error: Error) => ({ error }));
  for (let ready = false; !ready; ) ready = ((await fetch(`${first.base}/sessions/${key}/approvals`).then((r) => r.json())) as { approvals: unknown[] }).approvals.length > 0;

  await first.server.drain();
  const result = await waiting;
  assert.equal('outcome' in result ? noticeFor(result.outcome, false) : undefined, undefined, 'a drained wait is not a failed run');

  const next = await distributor(dataDir);
  // A stopped session resumes on its next request; the same requestId finds the original submission.
  await fetch(`${next.base}/sessions/${key}/invocations`, { method: 'POST', body: JSON.stringify(body) });
  let approvals: { approvals: Array<{ id: string }> };
  do approvals = (await fetch(`${next.base}/sessions/${key}/approvals`).then((r) => r.json())) as typeof approvals;
  while (!approvals.approvals.length);
  await fetch(`${next.base}/sessions/${key}/approvals/${approvals.approvals[0].id}`, { method: 'POST', body: JSON.stringify({ decision: 'approve', by: 'jf' }) });
  const done = await settled(`${next.base}/sessions/${key}/invocations`, { ...body, wait: true });
  assert.equal(done.status, 'done');
});

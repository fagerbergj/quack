import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { DatasetItem } from './langfuse.ts';
import { readSession, type SessionRecord } from './record.ts';

/** Stands in for an extension's service half during a replay: it serves the pinned world and captures deliveries. */
export type EvalService = { env: Record<string, string>; captured: unknown[]; close(): Promise<void> };
export type EvalServices = Record<string, (pin: JsonObject) => Promise<EvalService>>;
export type Candidate = { deployment: string; agent: string; env?: Record<string, string> };
export type ReplayResult = { item: DatasetItem; seconds: number; record: SessionRecord; captured: unknown[]; error?: string };

const RUNTIME = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));

async function freePort(): Promise<number> {
  const s = createServer().listen(0, '127.0.0.1');
  await new Promise((r) => s.once('listening', r));
  const { port } = s.address() as { port: number };
  await new Promise((r) => s.close(r));
  return port;
}

// Polls with one request id: dedupe answers with the run's status, so no request has to outlive a long review.
// A runtime that exits (a bad config path, a crash) ends the wait at once instead of at the deadline.
async function untilSettled(url: string, input: string, deadline: number, child: ChildProcess) {
  for (;;) {
    if (child.exitCode !== null) throw new Error(`runtime exited with ${child.exitCode}`);
    try {
      const r = (await (await fetch(`${url}/invocations`, { method: 'POST', body: JSON.stringify({ input, requestId: 'eval-1' }) })).json()) as { status: string };
      if (r.status === 'done' || r.status === 'unanswered') return;
    } catch {
      // not listening yet
    }
    if (Date.now() > deadline) throw new Error('replay timed out');
    await new Promise((res) => setTimeout(res, 2_000));
  }
}

// Node ends a crash dump with its version line, so the error line itself is the one that says what went wrong.
const crashLine = (stderr: string) => {
  const lines = stderr.trim().split('\n');
  return lines.find((l) => /^\w*Error\b/.test(l)) ?? lines.at(-1);
};

/** Replays one item in a fresh session on the candidate definition; nothing it does is delivered (EVL-2). */
export async function replay(item: DatasetItem, candidate: Candidate, services: EvalServices, timeoutMs = 30 * 60_000): Promise<ReplayResult> {
  const { inputs, pins } = item.input as { inputs: string[]; pins: Record<string, JsonObject> };
  const missing = Object.keys(pins).filter((k) => !services[k]);
  if (missing.length) throw new Error(`no eval service for pinned ${missing.join(', ')}`);
  const doubles = await Promise.all(Object.entries(pins).map(([k, pin]) => services[k](pin)));
  const dir = await mkdtemp(join(tmpdir(), 'quack-eval-'));
  const port = await freePort();
  const env = { ...process.env, ...candidate.env, ...Object.assign({}, ...doubles.map((d) => d.env)), QUACK_CONFIG: candidate.deployment, QUACK_AGENT: candidate.agent, QUACK_SESSION_DIR: dir, PORT: String(port) };
  const child = spawn(process.execPath, [RUNTIME], { env, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', (d) => (stderr = (stderr + d).slice(-2000)));
  const started = Date.now();
  let error: string | undefined;
  try {
    // Only the first input replays: later turns were the operator's answers to what the recorded run said.
    await untilSettled(`http://127.0.0.1:${port}`, inputs[0] ?? '', started + timeoutMs, child);
  } catch (err) {
    error = `${(err as Error).message}${stderr ? `: ${crashLine(stderr)}` : ''}`;
  } finally {
    if (child.exitCode === null) {
      const exited = new Promise((r) => child.once('exit', r));
      child.kill('SIGTERM');
      await exited;
    }
    await Promise.all(doubles.map((d) => d.close()));
  }
  const seconds = Math.round((Date.now() - started) / 1000);
  // The session dir holds a clone of the repository; it goes once read, so a 20-item run does not fill the disk.
  const record = await readSession(dir).finally(() => rm(dir, { recursive: true, force: true }));
  return { item, seconds, record, captured: doubles.flatMap((d) => d.captured), error };
}

import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { EvalService } from '@quack/sdk';
import { childPort } from '@quack/session';
import type { DatasetItem } from './langfuse.ts';
import { stopChild, untilSettled } from './driver.ts';
import { readSession, type SessionRecord } from './record.ts';

export type { EvalService };
export type EvalServices = Record<string, (pin: JsonObject) => Promise<EvalService>>;
export type Candidate = { deployment: string; agent: string; env?: Record<string, string> };
export type ReplayResult = { item: DatasetItem; seconds: number; record: SessionRecord; captured: unknown[]; error?: string };

const RUNTIME = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));

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
  const env = { ...process.env, ...candidate.env, ...Object.assign({}, ...doubles.map((d) => d.env)), QUACK_CONFIG: candidate.deployment, QUACK_AGENT: candidate.agent, QUACK_SESSION_DIR: dir, QUACK_SESSION_KEY: `eval:${item.id}:${randomUUID().slice(0, 8)}`, PORT: '0' };
  const child = spawn(process.execPath, [RUNTIME], { env, stdio: ['ignore', 'ignore', 'pipe', 'ipc'] });
  let stderr = '';
  child.stderr?.on('data', (d) => (stderr = (stderr + d).slice(-2000)));
  const started = Date.now();
  let error: string | undefined;
  try {
    // Only the first input replays: later turns were the operator's answers to what the recorded run said.
    const deadline = started + timeoutMs;
    const port = await childPort(child, 'runtime', deadline - Date.now());
    await untilSettled(`http://127.0.0.1:${port}`, { input: inputs[0] ?? '', requestId: 'eval-1' }, deadline, child);
  } catch (err) {
    error = `${(err as Error).message}${stderr ? `: ${crashLine(stderr)}` : ''}`;
  } finally {
    await stopChild(child);
    await Promise.all(doubles.map((d) => d.close()));
  }
  const seconds = Math.round((Date.now() - started) / 1000);
  // The session dir holds a clone of the repository; it goes once read, so a 20-item run does not fill the disk.
  const record = await readSession(dir).finally(() => rm(dir, { recursive: true, force: true }));
  return { item, seconds, record, captured: doubles.flatMap((d) => d.captured), error };
}

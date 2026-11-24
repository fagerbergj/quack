import { execFileSync, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { cpSync, mkdtempSync, rmSync } from 'node:fs';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { basename, dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { EvalAdapter, EvalService } from '@quack/sdk';
import { childPort } from '@quack/session';
import type { DatasetItem } from './langfuse.ts';
import { drive, stopChild, type Answered, type Policy } from './driver.ts';
import { readSession, type SessionRecord } from './record.ts';
import { adapterScores, servicesOf } from './adapters.ts';
import { linkTree } from './judge-replay.ts';

export type { EvalService };
export type EvalServices = Record<string, (pin: JsonObject) => Promise<EvalService>>;
/** `runtime` is the runtime entry to spawn, this tree's when unset. */
export type Candidate = { deployment: string; agent: string; env?: Record<string, string>; runtime?: string };
/** `codeScores` are the adapters' rubric code checks, scored while the session directory their evidence may read still exists; `asks` what the replay answered. */
export type ReplayResult = { item: DatasetItem; seconds: number; record: SessionRecord; captured: unknown[]; codeScores: Record<string, number>; asks: Answered[]; stopped: boolean; error?: string };
/** The item's input: recorded inputs in order, pinned docs, and the recorded answers to its asks. */
export type ItemInput = { inputs: string[]; pins: Record<string, JsonObject>; asks?: Policy };

const RUNTIME = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));
// A replay against a dead model fails within about 5 minutes so the runner moves on, instead of parking for the deployment's hours.
export const REPLAY_OUTAGE_HOURS = 5 / 60;

/** At `.` this tree's bundle and runtime; at another ref the bundle's directory from there, and each `guides` directory under
 * packages/ from there over a copy of this tree's packages, so the extension code stays this tree's. */
export function candidateAt(ref: string, deployment: string, agent: string, guides: string[] = []): Candidate {
  if (ref === '.') return { deployment, agent };
  const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
  const inTree = (path: string) => relative(top, resolve(path));
  const outside = guides.map(inTree).filter((g) => !g.startsWith('packages/'));
  if (outside.length) throw new Error(`guides must be directories under packages/: ${outside.join(', ')}`);
  const dir = mkdtempSync(join(tmpdir(), 'quack-eval-arm-'));
  // The runtime reads its tree lazily, so the copy must outlive this call.
  process.once('exit', () => rmSync(dir, { recursive: true, force: true }));
  const extract = (path: string) => execFileSync('tar', ['-x', '-C', dir], { input: execFileSync('git', ['archive', '--format=tar', ref, path], { cwd: top, maxBuffer: 1 << 30 }) });
  extract(dirname(inTree(agent)));
  const bundle = { deployment, agent: join(dir, inTree(agent)) };
  if (!guides.length) return bundle;
  cpSync(join(top, 'packages'), join(dir, 'packages'), { recursive: true, filter: (src) => basename(src) !== 'node_modules' });
  for (const g of guides.map(inTree)) {
    rmSync(join(dir, g), { recursive: true, force: true });
    extract(g);
  }
  linkTree(dir);
  return { ...bundle, runtime: join(dir, 'packages/runtime/src/main.ts') };
}

// Node ends a crash dump with its version line, so the error line itself is the one that says what went wrong.
const crashLine = (stderr: string) => {
  const lines = stderr.trim().split('\n');
  return lines.find((l) => /^\w*Error\b/.test(l)) ?? lines.at(-1);
};

/** Replays one item in a fresh session on the candidate definition; nothing it does is delivered (EVL-2). */
export async function replay(item: DatasetItem, candidate: Candidate, adapters: EvalAdapter[], timeoutMs = 30 * 60_000, policy?: Policy): Promise<ReplayResult> {
  const services = servicesOf(adapters);
  const { inputs, pins, asks } = item.input as ItemInput;
  const missing = Object.keys(pins).filter((k) => !services[k]);
  if (missing.length) throw new Error(`no eval service for pinned ${missing.join(', ')}`);
  const doubles = await Promise.all(Object.entries(pins).map(([k, pin]) => services[k](pin)));
  const dir = await mkdtemp(join(tmpdir(), 'quack-eval-'));
  // The runtime's server answers only its launch token, which it reads from this file and removes.
  const token = randomUUID();
  await writeFile(join(dir, '.launch-token'), token, { mode: 0o600 });
  const env = { ...process.env, QUACK_MODEL_OUTAGE_HOURS: String(REPLAY_OUTAGE_HOURS), ...candidate.env, ...Object.assign({}, ...doubles.map((d) => d.env)), QUACK_CONFIG: candidate.deployment, QUACK_AGENT: candidate.agent, QUACK_SESSION_DIR: dir, QUACK_SESSION_TOKEN_FILE: join(dir, '.launch-token'), QUACK_SESSION_KEY: `eval:${item.id}:${randomUUID().slice(0, 8)}`, PORT: '0' };
  const child = spawn(process.execPath, [candidate.runtime ?? RUNTIME], { env, stdio: ['ignore', 'ignore', 'pipe', 'ipc'] });
  let stderr = '';
  child.stderr?.on('data', (d) => (stderr = (stderr + d).slice(-2000)));
  const started = Date.now();
  let error: string | undefined;
  const answered: Answered[] = [];
  let stopped = false;
  try {
    const deadline = started + timeoutMs;
    const port = await childPort(child, 'runtime', deadline - Date.now());
    stopped = await drive({ url: `http://127.0.0.1:${port}`, token }, inputs, policy ?? asks ?? {}, answered, deadline, child);
  } catch (err) {
    error = `${(err as Error).message}${stderr ? `: ${crashLine(stderr)}` : ''}`;
  } finally {
    await stopChild(child);
    await Promise.all(doubles.map((d) => d.close()));
  }
  const seconds = Math.round((Date.now() - started) / 1000);
  // The session dir holds a clone of the repository; it goes once read, so a 20-item run does not fill the disk.
  try {
    const record = await readSession(dir);
    return { item, seconds, record, captured: doubles.flatMap((d) => d.captured), codeScores: await adapterScores(adapters, record), asks: answered, stopped, error };
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

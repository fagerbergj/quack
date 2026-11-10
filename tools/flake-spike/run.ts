import { spawn } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Case } from './cases.ts';

export const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const VITEST = 'node_modules/.bin/vitest';
const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

export type RunOpts = { randomize?: boolean; env?: Record<string, string>; timeoutMs?: number; preload?: string };
export type RunResult = { pass: boolean; ms: number; out: string; ran: boolean };

/** The argv that runs one case the way CI does: node:test with the leak check, or vitest with the case's config. */
export function command(c: Case, o: RunOpts): string[] {
  const t = c.name ? escape(c.name) : undefined;
  if (c.runner === 'node') {
    return [process.execPath, '--test', '--import', './tools/leak-check.ts', ...(o.preload ? ['--import', o.preload] : []), '--test-timeout=30000',
      ...(o.randomize ? ['--test-randomize'] : []), ...(t ? [`--test-name-pattern=${t}`] : []), c.file];
  }
  const web = c.runner === 'vitest-web';
  const config = join(ROOT, web ? 'tools/flake-spike/vitest.web.config.ts' : 'tools/flake-spike/vitest.config.ts');
  return [join(ROOT, VITEST), 'run', '--config', config, web ? c.file.replace('packages/web/', '') : c.file, ...(t ? ['-t', t] : []), ...(o.randomize ? ['--sequence.shuffle'] : [])];
}

// CI runs the web tests from their package (npm test -w @quack/web), and some read fixtures relative to it.
const cwdOf = (c: Case) => (c.runner === 'vitest-web' ? join(ROOT, 'packages/web') : ROOT);

// node:test prints "ℹ pass N"; vitest prints "Tests  N passed"; none means the name filter matched nothing or all failed.
const ranAny = (out: string) => /ℹ pass [1-9]|Tests\s+.*\d+ passed/.test(out);

/** Runs one case in a fresh process; a timeout kills its process group and counts as a failure. */
export function runCase(c: Case, o: RunOpts = {}): Promise<RunResult> {
  const [bin, ...args] = command(c, o);
  const started = Date.now();
  return new Promise((done) => {
    const child = spawn(bin, args, { cwd: cwdOf(c), env: { ...process.env, CI: '1', ...o.env }, detached: true, stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '';
    child.stdout.on('data', (b) => (out += b));
    child.stderr.on('data', (b) => (out += b));
    const killGroup = () => { try { process.kill(-child.pid!, 'SIGKILL'); } catch { /* gone */ } };
    const kill = setTimeout(killGroup, o.timeoutMs ?? 90_000);
    child.on('close', (code) => {
      clearTimeout(kill);
      killGroup(); // whatever the case spawned and left behind
      done({ pass: code === 0, ms: Date.now() - started, out: out.slice(-4000), ran: ranAny(out) });
    });
  });
}

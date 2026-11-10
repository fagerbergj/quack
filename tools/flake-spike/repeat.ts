// Detector C: each test file 10 times at once in random order, then once beside a CPU hog; files over 15 s are skipped.
// Usage: node tools/flake-spike/repeat.ts [prs]  (writes results/repeat[-prs].json)
import { spawn, type ChildProcess } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { availableParallelism } from 'node:os';
import { join } from 'node:path';
import type { Case } from './cases.ts';
import { atHead } from './mutate.ts';
import { ROOT, runCase, type RunResult } from './run.ts';
import { caseSet } from './static.ts';

const RUNS = 10;
const COSTLY_MS = 15_000;

/** The failing test names a run printed (node:test ✖ lines, vitest × lines). */
const failures = (r: RunResult) => [...new Set(r.out.split('\n').map((l) => /^\s*(?:✖|×)\s+(.+?)(?: \(\d[\d.]*m?s\))?$/.exec(l)?.[1]).filter((n): n is string => !!n && !n.startsWith('failing tests')))];

export type RepeatResult = { id: string; status: 'ok' | 'costly' | 'baseline-fail'; runs: number; failed: number; hogFailed: boolean; names: string[]; ms: number };

async function repeatOne(c: Case): Promise<RepeatResult> {
  const started = Date.now();
  const first = await runCase(c, { randomize: true });
  const base = { id: c.id, names: failures(first), hogFailed: false };
  if (!first.pass) return { ...base, status: 'baseline-fail', runs: 1, failed: 1, ms: Date.now() - started };
  if (first.ms > COSTLY_MS) return { ...base, status: 'costly', runs: 1, failed: 0, ms: Date.now() - started };
  const rest = await Promise.all(Array.from({ length: RUNS - 1 }, () => runCase(c, { randomize: true })));
  const bad = rest.filter((r) => !r.pass);
  return { ...base, status: 'ok', runs: RUNS, failed: bad.length, names: [...new Set(bad.flatMap(failures))], ms: Date.now() - started };
}

// One busy loop per core, so the run under it gets a fraction of the CPU it had.
function hog(): ChildProcess[] {
  return Array.from({ length: availableParallelism() }, () => spawn(process.execPath, ['-e', 'for(;;){}'], { stdio: 'ignore' }));
}

async function pool<T, R>(xs: T[], n: number, f: (x: T) => Promise<R>): Promise<R[]> {
  const out: R[] = [];
  let next = 0;
  await Promise.all(Array.from({ length: n }, async () => { while (next < xs.length) out.push(await f(xs[next++])); }));
  return out;
}

/** One case per test file for PR sets: a flake shows in the file run whichever test it is in. */
const units = (cases: Case[], prs: boolean): Case[] => (prs ? [...new Map(atHead(cases).map((c) => [c.file, { ...c, id: c.file, name: undefined }])).values()] : cases.filter((c) => c.runnable));

if (import.meta.main) {
  const { cases, out } = caseSet(process.argv[2]);
  const list = units(cases, process.argv[2] === 'prs');
  const started = Date.now();
  const results = await pool(list, 4, async (c) => {
    const r = await repeatOne(c);
    console.log(`${r.id} ${r.status} ${r.failed}/${r.runs} ${r.names.join(' | ').slice(0, 200)}`);
    return r;
  });
  const hogs = hog();
  try {
    const eligible = list.filter((c) => results.find((r) => r.id === c.id)?.status === 'ok');
    await pool(eligible, 4, async (c) => {
      const r = await runCase(c, { randomize: true, timeoutMs: 120_000 });
      const res = results.find((x) => x.id === c.id)!;
      res.runs++;
      if (!r.pass) Object.assign(res, { hogFailed: true, failed: res.failed + 1, names: [...new Set([...res.names, ...failures(r)])] });
      if (!r.pass) console.log(`under load: ${c.id} failed ${failures(r).join(' | ').slice(0, 200)}`);
    });
  } finally {
    for (const h of hogs) h.kill('SIGKILL');
  }
  mkdirSync(join(ROOT, 'tools/flake-spike/results'), { recursive: true });
  writeFileSync(join(ROOT, `tools/flake-spike/results/repeat${out}.json`), `${JSON.stringify({ ms: Date.now() - started, results }, null, 1)}\n`);
}

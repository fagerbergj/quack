// Scores each detector on the labeled corpus: per class, how many cases it flags in the right category, and its flags
// on the good set. Usage: node tools/flake-spike/score.ts  (reads results/*.json; prints markdown tables)
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { CASES, type Category } from './cases.ts';
import { ROOT } from './run.ts';

type Flags = Map<string, Set<'flaky' | 'taut'>>;
const load = (f: string) => {
  const p = join(ROOT, 'tools/flake-spike/results', f);
  return existsSync(p) ? JSON.parse(readFileSync(p, 'utf8')) : undefined;
};
const flag = (m: Flags, id: string, c: 'flaky' | 'taut') => m.set(id, (m.get(id) ?? new Set()).add(c));

type Detector = { name: string; flags: Flags; cost: string };
type Run = { ms: number; results: any[] };
const runs = (r: Run) => r.results.reduce((n: number, x: { runs: number }) => n + x.runs, 0);
const QS: Record<string, 'flaky' | 'taut'> = { time: 'flaky', port: 'flaky', shared: 'flaky', external: 'flaky', wait: 'flaky', order: 'flaky', broken: 'taut', mocks: 'taut', logic: 'taut' };

const LOADERS: Array<[string, (r: Run) => Detector]> = [
  ['static', (r) => {
    const f: Flags = new Map();
    for (const x of r.results) for (const h of x.hits) flag(f, x.id, h.category);
    return { name: 'A static', flags: f, cost: `${(r.ms / 1000).toFixed(1)} s, 0 model calls` };
  }],
  ['clef', (r) => {
    const f: Flags = new Map();
    let unsure = 0;
    for (const x of r.results) for (const [k, a] of Object.entries(x.answers) as Array<[string, { verdict: string }]>) {
      if (a.verdict === 'yes') flag(f, x.id, QS[k]);
      if (a.verdict === 'unsure') unsure++;
    }
    return { name: 'B Clef', flags: f, cost: `${(r.ms / 1000).toFixed(0)} s, ${r.results.length} Clef calls, ${unsure} unsure answers` };
  }],
  ['repeat', (r) => {
    const f: Flags = new Map();
    for (const x of r.results) if (x.failed > 0) flag(f, x.id, 'flaky');
    return { name: 'C repeat', flags: f, cost: `${(r.ms / 1000).toFixed(0)} s wall, ${runs(r)} runs` };
  }],
  ['mutate', (r) => {
    const f: Flags = new Map();
    for (const x of r.results) if (x.status === 'ok' && x.killed === 0) flag(f, x.id, 'taut');
    return { name: 'D mutation', flags: f, cost: `${(r.ms / 1000).toFixed(0)} s wall, ${runs(r)} runs` };
  }],
];

/** Per detector that has results, which categories it puts each case in, and what the whole run cost. */
export const detectors = (suffix = ''): Detector[] => LOADERS.flatMap(([f, make]) => {
  const r = load(`${f}${suffix}.json`);
  return r ? [make(r)] : [];
});

function table(category: Exclude<Category, 'good'>, ds: ReturnType<typeof detectors>): string {
  const classes = [...new Set(CASES.filter((c) => c.category === category).map((c) => c.cls))];
  const cell = (flags: Flags, ids: string[]) => `${ids.filter((id) => flags.get(id)?.has(category)).length}/${ids.length}`;
  const rows = classes.map((cls) => {
    const ids = CASES.filter((c) => c.category === category && c.cls === cls).map((c) => c.id);
    return `| ${cls} | ${ds.map((d) => cell(d.flags, ids)).join(' | ')} |`;
  });
  const all = CASES.filter((c) => c.category === category).map((c) => c.id);
  const good = CASES.filter((c) => c.category === 'good').map((c) => c.id);
  return [
    `| ${category} class | ${ds.map((d) => d.name).join(' | ')} |`, `|---|${ds.map(() => '---').join('|')}|`, ...rows,
    `| **recall** | ${ds.map((d) => `**${cell(d.flags, all)}**`).join(' | ')} |`,
    `| false positives on ${good.length} good | ${ds.map((d) => cell(d.flags, good)).join(' | ')} |`,
  ].join('\n');
}

if (import.meta.main) {
  const ds = detectors();
  console.log(table('flaky', ds), '\n');
  console.log(table('taut', ds), '\n');
  for (const d of ds) console.log(`- ${d.name}: ${d.cost}`);
}

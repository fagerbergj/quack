// Counts the fetched judged versions by kind and outcome. Usage: node tools/spike-fanout/inventory.ts
import { readdirSync, readFileSync } from 'node:fs';

const dir = 'tools/spike-fanout/data/all';
const rows: Array<{ key: string; kind: string; version: number; passed: boolean | undefined; score?: number; rubric?: string; len: number; gates: string[] }> = [];
for (const f of readdirSync(dir)) {
  const j = JSON.parse(readFileSync(`${dir}/${f}`, 'utf8'));
  for (const a of j.artifacts) rows.push({ key: j.key, kind: a.kind, version: a.version, passed: a.verdict?.passed, score: a.verdict?.score, rubric: a.verdict?.rubric, len: a.content?.length ?? 0, gates: a.verdict?.gatesFailed ?? [] });
}
const tally = new Map<string, number>();
for (const r of rows) tally.set(`${r.kind} ${r.rubric ?? 'no verdict'} passed=${r.passed}`, (tally.get(`${r.kind} ${r.rubric ?? 'no verdict'} passed=${r.passed}`) ?? 0) + 1);
for (const [k, n] of [...tally].sort()) console.log(n, k);
console.log('--- failed');
for (const r of rows.filter((x) => x.passed === false)) console.log(r.kind, r.key, `v${r.version}`, r.score?.toFixed(2), r.len, r.gates.join(','));

// Usage: node tools/memory-spike/report.ts > tables.md
// Prints the per-type accuracy table and every probe's raw answer per arm, from results/<arm>.json.
import { existsSync, readFileSync } from 'node:fs';

const HERE = new URL('.', import.meta.url).pathname;
type Row = { id: string; type: string; q: string; ref: string; answer: string; correct: boolean; wrongFact: boolean; abstained: boolean; by: string };
const arms = ['off', 'episodes', 'graphiti'].filter((a) => existsSync(`${HERE}results/${a}.json`));
const res = Object.fromEntries(arms.map((a) => [a, JSON.parse(readFileSync(`${HERE}results/${a}.json`, 'utf8')) as { probes: Row[]; usage: Record<string, number>; seconds: number; ingestSeconds: number }]));
const types = [...new Set(res[arms[0]].probes.map((p) => p.type))];
const pct = (rows: Row[], f: (r: Row) => boolean) => `${rows.filter(f).length}/${rows.length}`;

console.log(`| Probe type | ${arms.join(' | ')} |\n| --- |${arms.map(() => ' --- |').join('')}`);
for (const t of types) console.log(`| ${t} | ${arms.map((a) => pct(res[a].probes.filter((p) => p.type === t), (r) => r.correct)).join(' | ')} |`);
console.log(`| **all** | ${arms.map((a) => pct(res[a].probes, (r) => r.correct)).join(' | ')} |`);
const answerable = (a: string) => res[a].probes.filter((p) => p.type !== 'abstention');
console.log(`| wrong-fact answers (answered, and wrong) | ${arms.map((a) => pct(res[a].probes, (r) => r.wrongFact)).join(' | ')} |`);
console.log(`| abstained on an answerable probe | ${arms.map((a) => pct(answerable(a), (r) => r.abstained)).join(' | ')} |`);
console.log(`| reader+judge calls, seconds | ${arms.map((a) => `${res[a].usage.chat}, ${res[a].usage.chatSeconds.toFixed(0)} s`).join(' | ')} |`);
console.log(`| ingestion seconds | ${arms.map((a) => res[a].ingestSeconds.toFixed(0)).join(' | ')} |\n`);

for (const p of res[arms[0]].probes) {
  console.log(`**${p.id}** (${p.type}) ${p.q}  \nReference: ${p.ref}\n`);
  for (const a of arms) {
    const r = res[a].probes.find((x) => x.id === p.id)!;
    console.log(`- \`${a}\` ${r.correct ? 'correct' : r.wrongFact ? '**wrong fact**' : 'abstained'} (${r.by}): ${r.answer.replace(/\s+/g, ' ')}`);
  }
  console.log('');
}

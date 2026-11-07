// Lists the checks where the fan-out and the recorded verdict differ, with the recorded notes and the chunk answers, for adjudication.
// Usage: node tools/spike-fanout/disagree.ts <kind-file: contracts|sleeper|research|reviews>
import { readFileSync } from 'node:fs';

const runs = JSON.parse(readFileSync(`tools/spike-fanout/out/round2-${process.argv[2]}.json`, 'utf8')) as any[];
const p = (a: any) => (a?.p !== undefined ? a.p.toFixed(2) : a?.probs ? JSON.stringify(Object.fromEntries(Object.entries(a.probs).map(([k, v]: any) => [k, +v.toFixed(2)]))) : '-');
for (const r of runs.filter((x) => !x.skipped)) {
  for (const c of r.checks) {
    if (c.recordedFlagged === undefined || c.recordedFlagged === c.fanFlagged || c.check === 'sibling_unreported') continue;
    console.log(`\n### ${r.item.split('|')[1].replace('github:fagerbergj/quack', '')} v${r.version} ${r.passed ? 'PASS' : 'FAIL'} ${c.check}: recorded flagged=${c.recordedFlagged} (met ${c.recordedMet}, ${JSON.stringify(c.resolvers)}); fan flagged=${c.fanFlagged} decided=${c.decided} chunks=${c.chunks}`);
    console.log(`recorded notes: ${JSON.stringify(c.notes)?.slice(0, 700)}`);
    for (const it of c.items) console.log(`  [${p(it.a)}] ${it.text.replace(/\s+/g, ' ').slice(0, 230)}`);
  }
}

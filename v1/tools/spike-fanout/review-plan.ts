// Reads the review estimate (stdin: round2 estimate output) and prints the trigger statistics and a plan within a request budget.
// Usage: node tools/spike-fanout/round2.ts estimate | node tools/spike-fanout/review-plan.ts <budget>
import { readFileSync, writeFileSync } from 'node:fs';

type Row = { pr: string; version: number; fail: boolean; flagged: string[]; promises: number; sibling: number };
const rows: Row[] = readFileSync(0, 'utf8').split('\n').flatMap((l) => {
  const m = /^github\.review #(\d+) v(\d+) (FAIL|pass) recflag: (.*?) requests \d+ chunks promises_handled:(\d+) sibling_parts:(\d+)/.exec(l);
  return m ? [{ pr: m[1], version: Number(m[2]), fail: m[3] === 'FAIL', flagged: m[4] === '-' ? [] : m[4].split(','), promises: Number(m[5]), sibling: Number(m[6]) }] : [];
});
const failed = rows.filter((r) => r.fail);
const sib = failed.filter((r) => r.flagged.includes('sibling_parts'));
const pro = failed.filter((r) => r.flagged.includes('promises_handled'));
console.log(`failed versions ${failed.length}; sibling_parts flagged ${sib.length}, of which a new same-suffix file exists in ${sib.filter((r) => r.sibling > 0).length}; promises_handled flagged ${pro.length}`);
console.log(`sibling flagged as the only flagged model check: ${sib.filter((r) => r.flagged.length === 1).length}, of which triggered ${sib.filter((r) => r.flagged.length === 1 && r.sibling > 0).length}`);
const budget = Number(process.argv[2] ?? 100);
const plan: string[] = [];
let used = 0;
// Triggered sibling versions first (one request each), then promises-flagged failures, fewest chunks first.
for (const r of failed.filter((x) => x.sibling > 0 && x.flagged.includes('sibling_parts'))) { plan.push(`github.review|github:fagerbergj/quack#${r.pr}|${r.version}`); used += r.sibling + r.promises; }
for (const r of pro.filter((x) => !plan.includes(`github.review|github:fagerbergj/quack#${x.pr}|${x.version}`)).sort((a, b) => a.promises - b.promises)) { if (used + r.promises + r.sibling > budget) continue; plan.push(`github.review|github:fagerbergj/quack#${r.pr}|${r.version}`); used += r.promises + r.sibling; }
writeFileSync('tools/spike-fanout/out/plan-reviews.json', JSON.stringify(plan));
console.log(`plan ${plan.length} versions, about ${used} requests`);

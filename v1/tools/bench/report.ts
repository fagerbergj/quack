// Summarizes a run.ts output directory: v1 against v0 per PR, from each session's sealed reviews and judge verdicts.
// Usage: node tools/bench/report.ts <out-dir>
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels } from '@earendil-works/pi-ai';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { parseReview } from '@quack/github';

const V0_STATE: Record<string, string> = { APPROVED: 'approve', CHANGES_REQUESTED: 'request_changes', COMMENTED: 'comment' };
const outDir = process.argv[2];

async function judged(dir: string) {
  const s = await openSession({ dir, models: createModels(), agent: { model: { provider: 'none', modelId: 'none' } } });
  const versions = (await s.harness.snapshot(Artifacts, s.root.id, BACKGROUND_CONTEXT))?.kinds['github.review']?.versions ?? [];
  await s.close();
  const last = versions.at(-1);
  const v = (last?.verdict ?? {}) as { score?: number; passed?: boolean; limitReached?: boolean; checks?: Array<{ resolvers: Record<string, number> }> };
  const resolvers: Record<string, number> = {};
  for (const c of v.checks ?? []) for (const [k, n] of Object.entries(c.resolvers)) resolvers[k] = (resolvers[k] ?? 0) + n;
  return { versions: versions.length, content: last?.content ?? '', score: v.score, outcome: v.passed ? 'passed' : v.limitReached ? 'limit' : 'none', resolvers };
}

const rows: string[] = ['| PR | v0 | v1 | v0 blocking | v1 blocking | time | versions | score | outcome | resolvers |', '| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |'];
for (const name of readdirSync(outDir).filter((d) => d.startsWith('pr-')).sort()) {
  const r = JSON.parse(readFileSync(join(outDir, name, 'result.json'), 'utf8'));
  const j = await judged(join(outDir, name, 'session'));
  const review = parseReview(j.content);
  const v0Blocking = Number(/(\d+) blocking/.exec(r.v0.body)?.[1] ?? 0);
  const v1Blocking = review.findings.filter((f) => f.label === 'blocking').length;
  const res = Object.entries(j.resolvers).map(([k, n]) => `${k} ${n}`).join(', ');
  rows.push(`| #${r.pr} | ${V0_STATE[r.v0.state] ?? r.v0.state} | ${review.verdict ?? r.result?.status} | ${v0Blocking} | ${v1Blocking} | ${r.seconds}s | ${j.versions} | ${j.score?.toFixed(2) ?? '-'} | ${j.outcome} | ${res} |`);
}
console.log(rows.join('\n'));

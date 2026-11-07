// Pairs each session's judge-pass tasks with its judged versions (by order, when the counts match) and reports seconds per recorded 27B escalation.
// Usage: node tools/spike-fanout/inflation.ts
import { readFileSync } from 'node:fs';

const tasks = readFileSync('tools/spike-fanout/out/times.jsonl', 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l)) as Array<{ key: string; name: string; startedAt: number; endedAt: number }>;
const sel = JSON.parse(readFileSync('tools/spike-fanout/out/selection.json', 'utf8')) as Array<{ key: string; kind: string; version: number }>;
const TOOL: Record<string, string> = { 'github.review': 'submit_review', 'contract.spec': 'submit_contract', 'research.report': 'submit_report' };
const rows: Array<{ kind: string; s: number; escalations: number; perEsc: number }> = [];
let skipped = 0;
for (const key of [...new Set(sel.map((r) => r.key))]) {
  const f = `tools/spike-fanout/data/all/${key.replace(/\W+/g, '_')}.json`;
  let doc: { artifacts: Array<{ kind: string; version: number; verdict?: { checks: Array<{ resolvers: Record<string, number> }> } }> };
  try { doc = JSON.parse(readFileSync(f, 'utf8')); } catch { continue; }
  for (const kind of Object.keys(TOOL)) {
    const versions = doc.artifacts.filter((a) => a.kind === kind && a.verdict);
    const passes = tasks.filter((t) => t.key === key && t.name === TOOL[kind] && t.endedAt - t.startedAt >= 3000).sort((a, b) => a.startedAt - b.startedAt);
    if (!versions.length) continue;
    if (versions.length !== passes.length) { skipped++; continue; }
    versions.forEach((v, i) => {
      const esc = v.verdict!.checks.reduce((n, c) => n + (c.resolvers['agent-model'] ?? 0) + (c.resolvers['judge-model'] ?? 0), 0);
      const s = (passes[i].endedAt - passes[i].startedAt) / 1000;
      rows.push({ kind, s, escalations: esc, perEsc: esc ? s / esc : NaN });
    });
  }
}
const med = (a: number[]) => [...a].sort((x, y) => x - y)[Math.floor((a.length - 1) / 2)];
for (const kind of Object.keys(TOOL)) {
  const rs = rows.filter((r) => r.kind === kind);
  const withEsc = rs.filter((r) => r.escalations > 0);
  if (!rs.length) continue;
  console.log(`${kind}: paired ${rs.length} passes (${skipped} sessions unpaired); median ${med(rs.map((r) => r.s)).toFixed(0)} s; with 27B escalations ${withEsc.length}, median ${med(withEsc.map((r) => r.escalations))} escalations and ${withEsc.length ? med(withEsc.map((r) => r.perEsc)).toFixed(0) : 'n/a'} s per escalation; over 60 s per escalation (queue-inflated): ${withEsc.filter((r) => r.perEsc > 60).length}; passes with no escalation: ${rs.length - withEsc.length} (median ${(med(rs.filter((r) => !r.escalations).map((r) => r.s)) ?? 0).toFixed(0)} s)`);
}

// Picks the round-2 set: failed versions where a covered model check was flagged, then the other failed ones, then passes. Writes out/selection.json.
// Usage: node tools/spike-fanout/select.ts
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';

const COVERED: Record<string, string[]> = {
  'github.review': ['promises_handled', 'sibling_parts'],
  'research.report': ['answers_every_ask', 'summary_follows_findings', 'no_self_contradiction', 'supported_by_source', 'contradicted_by_source'],
  'contract.spec': ['meets_intent', 'follows_guide', 'adds_unasked'],
  'sleeper.cards': [],
};
const dir = 'tools/spike-fanout/data/all';
type Row = { key: string; kind: string; version: number; passed: boolean; score: number; flagged: string[]; gates: string[]; resolvers: string[] };
const rows: Row[] = [];
for (const f of readdirSync(dir)) {
  const j = JSON.parse(readFileSync(`${dir}/${f}`, 'utf8'));
  for (const a of j.artifacts) {
    if (!a.verdict || !(a.kind in COVERED)) continue;
    const checks = a.verdict.checks as Array<{ id: string; weight: number | string; met: number | null; resolvers: Record<string, number> }>;
    const cover = a.kind === 'sleeper.cards' ? checks.filter((c) => c.units > 0 && c.resolvers && Object.keys(c.resolvers).some((r) => r !== 'code')) : checks.filter((c) => COVERED[a.kind].includes(c.id));
    const flagged = cover.filter((c) => c.met !== null && (typeof c.weight === 'number' && c.weight < 0 ? c.met > 0 : c.met < 1)).map((c) => c.id);
    rows.push({ key: j.key, kind: a.kind, version: a.version, passed: a.verdict.passed, score: a.verdict.score, flagged, gates: a.verdict.gatesFailed, resolvers: [...new Set(cover.flatMap((c) => Object.keys(c.resolvers ?? {})))] });
  }
}
const by = (k: string) => rows.filter((r) => r.kind === k);
for (const k of Object.keys(COVERED)) {
  const rs = by(k);
  console.log(k, 'versions', rs.length, 'failed', rs.filter((r) => !r.passed).length, 'failed with a covered check flagged', rs.filter((r) => !r.passed && r.flagged.length).length, 'passed with one flagged', rs.filter((r) => r.passed && r.flagged.length).length);
}
writeFileSync('tools/spike-fanout/out/selection.json', JSON.stringify(rows));
for (const r of rows.filter((x) => x.kind !== 'github.review' && !x.passed)) console.log(r.kind, r.key.slice(0, 40), `v${r.version}`, r.score.toFixed(2), 'flagged:', r.flagged.join(','), 'gates:', r.gates.join(','));

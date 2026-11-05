// Lists failed review versions where promises_handled or sibling_parts was flagged, with the review's size. Usage: node tools/spike-fanout/list-reviews.ts
import { readFileSync } from 'node:fs';

const rows = JSON.parse(readFileSync('tools/spike-fanout/out/selection.json', 'utf8')) as Array<{ key: string; kind: string; version: number; passed: boolean; score: number; flagged: string[]; gates: string[] }>;
for (const r of rows.filter((x) => x.kind === 'github.review' && !x.passed && x.flagged.length)) console.log(r.key.split('#')[1], `v${r.version}`, r.score.toFixed(2), r.flagged.join(','), 'gates:', r.gates.join(',') || '-');

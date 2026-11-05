// Prints the units a scope makes for one artifact, to eyeball a chunker before spending Clef requests.
// Usage: node tools/spike-fanout/inspect.ts research <case> <scope> | review <n> <scope>
import { readdirSync, readFileSync } from 'node:fs';
import { research, review } from './declarations.ts';
import { unitsOf, type Inputs } from './strategies.ts';
import { researchCases } from '../research/cases.ts';

const [kind, id, scope] = process.argv.slice(2);
let inputs: Inputs;
if (kind === 'prod') {
  const f = readdirSync('tools/spike-fanout/data').find((x) => x.startsWith('report-') && x.includes(id))!;
  const j = JSON.parse(readFileSync(`tools/spike-fanout/data/${f}`, 'utf8'));
  inputs = { report: j.report, request: j.show.messages.find((m: any) => m.role === 'user').text };
} else if (kind === 'research') {
  const c = researchCases().find((x) => x.case === id)!;
  inputs = { report: c.content, request: (c.evidence.request![0].state as { request: string }).request };
} else {
  const r = JSON.parse(readFileSync(`tools/spike-fanout/data/review-${id}.json`, 'utf8'));
  inputs = { description: `${r.pr.title}\n\n${r.pr.body}`, issues: '', diff: r.diff, diffW: r.diffW, review: r.review, repoFiles: JSON.stringify(r.repoFiles ?? []) };
}
const decl = kind === 'review' ? review : research;
const sp = unitsOf(decl.scopes[scope], inputs);
console.log(`splitter: ${sp.splitter} ${sp.reason ?? ''}`);
for (const u of sp.units) console.log(`--- ${u.id} ${JSON.stringify(u.related)}\n${Object.entries(u.state).map(([k, v]) => `[${k}] ${v.slice(0, 500)}`).join('\n')}`);

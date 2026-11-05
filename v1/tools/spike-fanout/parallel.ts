// Concurrency measurement: one artifact's chunk requests sent one at a time (fresh, uncached) and then four at a time, wall clock both ways.
// Usage: node tools/spike-fanout/parallel.ts <kind|key|version> <concurrency>
import { readFileSync } from 'node:fs';
import { ask, requestsSent, stop } from './clef.ts';
import { contracts } from './declarations.ts';
import { unitsOf } from './strategies.ts';

const [item, concArg] = process.argv.slice(2);
const [, key, version] = item.split('|');
const art = JSON.parse(readFileSync(`tools/spike-fanout/data/all/${key.replace(/\W+/g, '_')}.json`, 'utf8')).artifacts.find((a: { version: number }) => a.version === Number(version));
const spec = JSON.parse(art.content);
const guide = readFileSync(`packages/contracts/skills/${spec.kind === 'rest' ? 'rest' : 'json-schema'}.md`, 'utf8');
const inputs = { guarantees: JSON.stringify(spec.intent.guarantees), choices: JSON.stringify(spec.choices), document: JSON.stringify(spec.document), guide, intent: `Purpose: ${spec.intent.purpose}\nNon-goals: ${spec.intent.nonGoals.join('; ')}\nGuarantees: ${spec.intent.guarantees.join('; ')}` };
const decl = contracts[spec.kind as 'rest' | 'json-schema'];
const jobs = Object.entries(decl.scopes).flatMap(([sid, scope]) => {
  const questions = Object.fromEntries(Object.entries(decl.checks).filter(([, c]) => c.scope === sid).map(([id, c]) => [id, c.question]));
  return unitsOf(scope, inputs).units.map((u) => ({ state: u.state, questions }));
});
const conc = Number(concArg);
let next = 0;
const t0 = performance.now();
const times: number[] = [];
await Promise.all(Array.from({ length: conc }, async () => {
  while (next < jobs.length) {
    const j = jobs[next++];
    times.push((await ask(j.state, j.questions, { par: conc > 1, fresh: true })).ms);
  }
}));
const wall = performance.now() - t0;
console.log(JSON.stringify({ item, requests: jobs.length, concurrency: conc, wallMs: Math.round(wall), summedClefMs: times.reduce((a, b) => a + b, 0), medianMs: [...times].sort((a, b) => a - b)[Math.floor(times.length / 2)], sent: requestsSent() }));
stop();

// Usage: node tools/memory-spike/run.ts <off|episodes|graphiti|mem0>
// Answers every probe with the same reader, prompt and cap; graphiti's and mem0's sections come from their ingest.py.
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import type { Episode } from './episodes.ts';
import { chat, usage, waitForSlots } from './llm.ts';
import { build, recall, tokens } from './store.ts';

export const CAP_TOKENS = 2000;
const HERE = new URL('.', import.meta.url).pathname;
type Probe = { id: string; type: string; q: string; ref: string; all?: string[]; any?: string[]; judge?: boolean; abstain?: boolean };

const READER = `You answer questions about past work on the GitHub repository fagerbergj/quack, using only the memory section in the message.
The memory holds notes from earlier sessions; it can be incomplete, and when it gives several values for the same thing over time, the most recent one holds.
If the memory does not contain the answer, reply exactly: not known
Answer in at most two sentences.`;

const JUDGE = `You grade an answer against a reference answer. The answer is CORRECT when it states the same facts as the reference, without contradicting it; a missing minor detail is fine. Reply with one word: CORRECT or WRONG.`;

const ABSTAINED = /\bnot known\b|\bunknown\b|does not (say|contain|mention)|no information/i;
const has = (a: string, s: string) => a.toLowerCase().includes(s.toLowerCase());

async function score(p: Probe, answer: string) {
  const abstained = ABSTAINED.test(answer) && answer.length < 200;
  if (p.abstain) return { correct: abstained, wrongFact: !abstained, abstained, by: 'code' };
  let correct: boolean;
  let by = 'code';
  if (p.judge && !abstained) {
    await waitForSlots();
    correct = /^CORRECT/i.test(await chat(JUDGE, `Question: ${p.q}\nReference: ${p.ref}\nAnswer: ${answer}`, 5));
    by = 'judge';
  } else correct = !abstained && (p.all ?? []).every((s) => has(answer, s)) && (!p.any || p.any.some((s) => has(answer, s)));
  return { correct, wrongFact: !abstained && !correct, abstained, by };
}

async function memoryFor(arm: string, probes: Probe[], prefix: Episode[]) {
  if (arm === 'off') return { sections: Object.fromEntries(probes.map((p) => [p.id, ''])), ingestSeconds: 0, rows: 0 };
  if (arm === 'graphiti' || arm === 'mem0') return JSON.parse(await readFile(`${HERE}results/${arm}-recall.json`, 'utf8'));
  const t = Date.now();
  const { db, passages } = await build(prefix);
  const ingestSeconds = (Date.now() - t) / 1000;
  const sections: Record<string, string> = {};
  for (const p of probes) sections[p.id] = (await recall(db, p.q, CAP_TOKENS)).join('\n\n---\n\n');
  return { sections, ingestSeconds, rows: passages };
}

if (import.meta.main) {
  const arm = process.argv[2];
  const { case: c, probes } = JSON.parse(await readFile(`${HERE}probes.json`, 'utf8')) as { case: { prefix: string[] }; probes: Probe[] };
  const all = (await readFile(`${HERE}data/episodes-all.jsonl`, 'utf8')).trim().split('\n').map((l) => JSON.parse(l) as Episode);
  const prefix = c.prefix.map((n) => all.find((e) => e.key.endsWith(n))!);
  const started = Date.now();
  const mem = await memoryFor(arm, probes, prefix);
  const rows = [];
  for (const p of probes) {
    const section = mem.sections[p.id] ?? '';
    if (tokens(section) > CAP_TOKENS * 1.05) throw new Error(`${arm} ${p.id}: memory section over the cap (${tokens(section)} tokens)`);
    await waitForSlots();
    const answer = await chat(READER, `<memory>\n${section || '(empty)'}\n</memory>\n\nQuestion: ${p.q}`);
    const s = await score(p, answer);
    rows.push({ id: p.id, type: p.type, q: p.q, ref: p.ref, answer, ...s, memoryTokens: tokens(section) });
    console.log(`${p.id} ${s.correct ? 'ok ' : 'NO '} ${answer.replace(/\n/g, ' ').slice(0, 140)}`);
  }
  await mkdir(`${HERE}results`, { recursive: true });
  const out = { arm, capTokens: CAP_TOKENS, ingestSeconds: mem.ingestSeconds, storedRows: mem.rows, ingest: mem.ingest, usage, seconds: (Date.now() - started) / 1000, probes: rows, sections: mem.sections };
  await writeFile(`${HERE}results/${arm}.json`, JSON.stringify(out, null, 2));
}

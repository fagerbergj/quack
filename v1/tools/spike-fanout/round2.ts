// Round 2: runs every candidate that applies to a recorded artifact version and compares each check with the recorded verdict.
// Usage: node tools/spike-fanout/round2.ts estimate | run <plan.json>   (plan: ["kind|key|version", ...]; DRY=1 never calls Clef)
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { requestsSent, stop } from './clef.ts';
import { contracts, research, review, sleeper } from './declarations.ts';
import { evaluate, pass } from './evaluate.ts';
import type { Declaration, Inputs } from './strategies.ts';

const dir = 'tools/spike-fanout/data';
const file = (key: string) => `${dir}/all/${key.replace(/\W+/g, '_')}.json`;
type Art = { kind: string; version: number; content: string; verdict: { passed: boolean; score: number; rubric: string; checks: Array<{ id: string; weight: number | string; met: number | null; resolvers: Record<string, number>; notes: string[]; units: number }> } };

function build(kind: string, key: string, art: Art): { decl: Declaration; inputs: Inputs } | string {
  if (kind === 'github.review') {
    const f = `${dir}/all/pr-${key.split('#')[1]}.json`;
    if (!existsSync(f)) return 'no PR data';
    const pr = JSON.parse(readFileSync(f, 'utf8'));
    return { decl: review, inputs: { description: `${pr.pr.title}\n\n${pr.pr.body}`, issues: '', diff: pr.diff, diffW: pr.diffW, review: art.content, repoFiles: JSON.stringify(pr.repoFiles) } };
  }
  if (kind === 'research.report') {
    const f = `${dir}/report-${key.replace(/\W+/g, '_')}.json`;
    const request = existsSync(f) ? (JSON.parse(readFileSync(f, 'utf8')).show.messages.find((m: { role: string }) => m.role === 'user')?.text ?? '') : '';
    return { decl: research, inputs: { report: art.content, request } };
  }
  if (kind === 'contract.spec') {
    const spec = JSON.parse(art.content);
    const guide = readFileSync(`packages/contracts/skills/${spec.kind === 'rest' ? 'rest' : 'json-schema'}.md`, 'utf8');
    return { decl: contracts[spec.kind as 'rest' | 'json-schema'], inputs: { guarantees: JSON.stringify(spec.intent.guarantees), choices: JSON.stringify(spec.choices), document: JSON.stringify(spec.document), guide, intent: `Purpose: ${spec.intent.purpose}\nNon-goals: ${spec.intent.nonGoals.join('; ')}\nGuarantees: ${spec.intent.guarantees.join('; ')}` } };
  }
  const decl = sleeper[art.verdict.rubric.split('@')[0]];
  return decl ? { decl, inputs: { card: art.content } } : `no declaration for ${art.verdict.rubric}`;
}

const flagged = (c: { weight: number | string; met: number | null }) => (c.met === null ? undefined : typeof c.weight === 'number' && c.weight < 0 ? c.met > 0 : c.met < 1);

async function one(item: string) {
  const [kind, key, v] = item.split('|');
  const art: Art = JSON.parse(readFileSync(file(key), 'utf8')).artifacts.find((a: Art) => a.kind === kind && a.version === Number(v));
  const built = build(kind, key, art);
  if (typeof built === 'string') return { item, skipped: built };
  const t0 = performance.now();
  const runs = await evaluate(built.decl, `${key}@${v}`, built.inputs, !process.env.DRY);
  const wall = performance.now() - t0;
  const checks = runs.map((r) => {
    const c = built.decl.checks[r.check];
    const rec = art.verdict.checks.find((x) => x.id === (c.records ?? r.check));
    return { check: r.check, chunks: r.chunks, decided: r.folded.value !== undefined, fanFlagged: r.folded.guess === c.flagWhen, fanFlaggedDecided: r.folded.value === undefined ? undefined : r.folded.value === c.flagWhen, recordedFlagged: rec ? flagged(rec) : undefined, recordedMet: rec?.met, resolvers: rec?.resolvers, notes: rec?.notes?.slice(0, 3), unknown: r.folded.unknown, splitter: r.splitter, tokens: r.tokens, oversize: r.oversize, ms: r.ms, items: r.texts.map((t, i) => ({ text: t.slice(0, 400), a: r.answers[i] })) };
  });
  return { item, kind, key, version: Number(v), passed: art.verdict.passed, score: art.verdict.score, rubric: art.verdict.rubric, pass: { ...pass, wallMs: wall }, checks };
}

const [mode, planFile] = process.argv.slice(2);
if (mode === 'estimate') {
  process.env.DRY = '1';
  const sel = JSON.parse(readFileSync('tools/spike-fanout/out/selection.json', 'utf8')) as Array<{ key: string; kind: string; version: number; passed: boolean; flagged: string[] }>;
  for (const r of sel) {
    if (!(process.env.KINDS ?? 'github.review|research.report|contract.spec|sleeper.cards').split('|').includes(r.kind)) continue;
    const out: any = await one(`${r.kind}|${r.key}|${r.version}`);
    console.log(r.kind, r.key.replace('github:fagerbergj/quack', '').replace('sleeper:1356407594683482112:', 'sl:'), `v${r.version}`, r.passed ? 'pass' : 'FAIL', 'recflag:', r.flagged.join(',') || '-', out.skipped ? `skipped ${out.skipped}` : `requests ${out.pass.requests} chunks ${out.checks.map((c: any) => `${c.check}:${c.chunks}`).join(' ')}`);
  }
}
if (mode === 'run') {
  const plan: string[] = JSON.parse(readFileSync(planFile, 'utf8'));
  const outFile = `tools/spike-fanout/out/round2-${process.env.OUT ?? 'a'}.json`;
  const done: any[] = existsSync(outFile) ? JSON.parse(readFileSync(outFile, 'utf8')) : [];
  for (const item of plan) {
    if (done.some((d) => d.item === item)) continue;
    done.push(await one(item));
    writeFileSync(outFile, JSON.stringify(done));
    console.error(`${item} requests so far ${requestsSent()}`);
  }
}
stop();

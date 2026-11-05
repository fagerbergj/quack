// Runs a declaration over artifacts against Clef and writes per-check measurements to out/<mode>.json.
// Usage: node tools/spike-fanout/run.ts research|baseline|review|prod-stats
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { ask, requestsSent, stop } from './clef.ts';
import { evaluate } from './evaluate.ts';
import { claims, contracts, research, review, splitters } from './declarations.ts';
import { askSplitFor, PROMISES } from './llm-splits.ts';
import type { Inputs } from './strategies.ts';
import { researchCases } from '../research/cases.ts';

const REQUESTION = ' Judge only from the text shown; a fact you do not recognise is not evidence of error.';
const rubricAsks = `Is every distinct ask and constraint in the request (place, timeframe, budget, audience) answered in the report's Summary and Findings, or named under Assumptions as unsettled?${REQUESTION}`;
const rubricSummary = `Does every claim in the Summary follow from a Finding?${REQUESTION}`;
const rubricSelf = `Does any figure, date or name in the report conflict with another passage of the report?${REQUESTION}`;
const bool = (instructions: string) => ({ type: 'bool' as const, instructions });

const mode = process.argv[2];
const cases = researchCases();
const results: any = {};

if (mode === 'research') {
  results.runs = [];
  for (const c of cases) {
    const inputs = { report: c.content, request: (c.evidence.request![0].state as { request: string }).request };
    results.runs.push(...(await evaluate(research, c.case, inputs, !process.env.DRY)));
    console.error(`${c.case} done, clef requests so far ${requestsSent()}`);
  }
}

if (mode === 'claims') {
  results.runs = [];
  for (const c of cases) {
    const source = c.evidence.claim?.[0]?.state as { sources: Array<{ text: string }> } | undefined;
    results.runs.push(...(await evaluate(claims, c.case, { report: c.content, source: source?.sources.map((s) => s.text).join('\n') ?? '' }, !process.env.DRY)));
    console.error(`${c.case} done, clef requests so far ${requestsSent()}`);
  }
}

if (mode === 'baseline') {
  // Today's whole-artifact path as the judge builds it: the request scope, then the review scope with both report-wide questions in one request.
  results.runs = [];
  for (const c of cases) {
    const state = c.evidence.request![0].state as { request: string; report: string };
    const a = await ask(state, { answers_every_ask: bool(rubricAsks) });
    const b = await ask({ report: c.content }, { summary_follows_findings: bool(rubricSummary), no_self_contradiction: bool(rubricSelf) });
    results.runs.push({ case: c.case, asks: a, review: b });
    console.error(`${c.case} done`);
  }
}

if (mode === 'review') {
  const dir = 'tools/spike-fanout/data';
  const picked = process.argv.slice(3);
  results.runs = [];
  for (const n of picked) {
    const r = JSON.parse(readFileSync(`${dir}/review-${n}.json`, 'utf8'));
    const inputs: Inputs = {
      description: `${r.pr.title}\n\n${r.pr.body}`,
      issues: r.issues.map((i: any) => `#${i.number} ${i.title}\n${i.body}`).join('\n\n'),
      diff: r.diff,
      diffW: r.diffW,
      review: r.review,
      repoFiles: JSON.stringify(r.repoFiles ?? []),
    };
    const wholeTokens = Math.ceil(JSON.stringify({ review: r.review, description: inputs.description, linkedIssues: inputs.issues, diff: r.diff.slice(0, 120_000) }).length / BYTES_PER_TOKEN);
    const checks = await evaluate(review, `#${n}`, inputs, !process.env.DRY);
    results.runs.push({ n, wholeTokens, recorded: Object.fromEntries(r.verdict.checks.filter((c: any) => ['promises_handled', 'sibling_parts', 'no_silent_approval', 'reports_every_defect'].includes(c.id)).map((c: any) => [c.id, { met: c.met, resolvers: c.resolvers, notes: c.notes }])), checks });
    console.error(`#${n} done, clef requests so far ${requestsSent()}`);
  }
}

if (mode === 'splitters') {
  const dir = 'tools/spike-fanout/data';
  results.runs = [];
  const asksOf = async (name: string, report: string, request: string, only: RegExp) => {
    const all = await evaluate(splitters, name, { report, request, 'llm:asks': JSON.stringify(askSplitFor(request)) }, !process.env.DRY);
    return all.filter((r) => only.test(r.check));
  };
  for (const c of cases) results.runs.push(...(await asksOf(c.case, c.content, (c.evidence.request![0].state as { request: string }).request, /^answers/)));
  for (const k of process.argv.slice(3).filter((a) => a.startsWith('p:')).map((a) => a.slice(2))) {
    const f = readdirSync(dir).find((x) => x.startsWith('report-') && x.includes(k))!;
    const j = JSON.parse(readFileSync(`${dir}/${f}`, 'utf8'));
    results.runs.push(...(await asksOf(`prod-${k}`, j.report, j.show.messages.find((m: any) => m.role === 'user').text, /^answers/)));
    console.error(`${k} done, clef requests so far ${requestsSent()}`);
  }
  for (const n of process.argv.slice(3).filter((a) => !a.startsWith('p:'))) {
    const r = JSON.parse(readFileSync(`${dir}/review-${n}.json`, 'utf8'));
    const inputs: Inputs = { description: `${r.pr.title}\n\n${r.pr.body}`, issues: '', diff: r.diff, diffW: r.diffW, review: r.review, repoFiles: '[]', 'llm:promises': JSON.stringify(PROMISES[Number(n)] ?? []) };
    results.runs.push(...(await evaluate(splitters, `#${n}`, inputs, !process.env.DRY)).filter((x) => /^promises/.test(x.check)));
    console.error(`#${n} done, clef requests so far ${requestsSent()}`);
  }
}

if (mode === 'contracts') {
  const dir = 'tools/spike-fanout/data';
  results.runs = [];
  for (const key of process.argv.slice(3)) {
    const art = JSON.parse(readFileSync(`${dir}/contract-${key.replace(/\W+/g, '_')}.json`, 'utf8'));
    const spec = JSON.parse(art.content);
    const guide = readFileSync(`packages/contracts/skills/${spec.kind === 'rest' ? 'rest' : 'json-schema'}.md`, 'utf8');
    const inputs: Inputs = { guarantees: JSON.stringify(spec.intent.guarantees), choices: JSON.stringify(spec.choices), document: JSON.stringify(spec.document), guide, intent: `Purpose: ${spec.intent.purpose}\nNon-goals: ${spec.intent.nonGoals.join('; ')}\nGuarantees: ${spec.intent.guarantees.join('; ')}` };
    const recorded = Object.fromEntries(art.verdict.checks.filter((c: any) => ['meets_intent', 'follows_guide', 'adds_unasked'].includes(c.id)).map((c: any) => [c.id, { met: c.met, resolvers: c.resolvers, notes: c.notes }]));
    results.runs.push({ key, kind: spec.kind, recorded, checks: await evaluate(contracts[spec.kind as 'rest' | 'json-schema'], key.slice(10, 18), inputs, !process.env.DRY) });
    console.error(`${key} done, clef requests so far ${requestsSent()}`);
  }
}

if (mode === 'prod-stats') {
  const dir = 'tools/spike-fanout/data';
  results.runs = [];
  const live = process.argv.slice(3);
  for (const f of readdirSync(dir).filter((x) => x.startsWith('report-'))) {
    const j = JSON.parse(readFileSync(`${dir}/${f}`, 'utf8'));
    const request = j.show.messages.find((m: any) => m.role === 'user')?.text ?? '';
    results.runs.push(...(await evaluate(research, f.replace(/^report-|\.json$/g, ''), { report: j.report, request }, live.some((k) => f.includes(k)))));
  }
}

writeFileSync(`tools/spike-fanout/out/${mode}${process.env.OUT ?? ''}.json`, JSON.stringify(results));
console.error(`clef requests sent: ${requestsSent()}`);
stop();

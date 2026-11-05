// Renders the measurements in out/*.json as the report's markdown tables. Usage: node tools/spike-fanout/tables.ts
import { readFileSync } from 'node:fs';
import { researchCases } from '../research/cases.ts';
import { ACT } from './strategies.ts';
import { ASKS, PROMISES } from './llm-splits.ts';
import type { CheckRun } from './run.ts';

const ESCALATION_S = 25;
const load = (m: string) => JSON.parse(readFileSync(`tools/spike-fanout/out/${m}.json`, 'utf8')).runs;
const med = (a: number[]) => (a.length ? [...a].sort((x, y) => x - y)[Math.floor((a.length - 1) / 2)] : 0);
const max = (a: number[]) => Math.max(0, ...a);
const sum = (a: number[]) => a.reduce((x, y) => x + y, 0);
const pct = (n: number, d: number) => (d ? `${Math.round((100 * n) / d)}% (${n}/${d})` : 'n/a');
const s1 = (ms: number) => (ms / 1000).toFixed(1);
const mm = (a: number[], f = (x: number) => String(x)) => `${f(med(a))} / ${f(max(a))}`;

/** Clef seconds for the artifact, plus the 27B escalations its undecided chunks would need when the check stays undecided. */
const clefMs = (r: CheckRun) => sum(r.ms);
const unknownChunks = (r: CheckRun) => r.decided.filter((d) => !d).length;
const withEscalation = (r: CheckRun) => clefMs(r) / 1000 + (r.folded.value === undefined ? unknownChunks(r) * ESCALATION_S : 0);

function shape(runs: CheckRun[], today?: string) {
  const live = runs.filter((r) => r.chunks > 0);
  const tokens = live.flatMap((r) => r.tokens);
  const ms = live.flatMap((r) => r.ms).filter((x) => x > 0);
  const answers = live.reduce((a, r) => a + r.chunks, 0);
  const decided = live.reduce((a, r) => a + r.decided.filter(Boolean).length, 0);
  return [
    `| chunks per artifact (median / max) | ${mm(runs.map((r) => r.chunks))} |`,
    `| tokens per chunk (median / max, cap 8192) | ${mm(tokens)} |`,
    `| oversize chunks (escalate unasked) | ${live.reduce((a, r) => a + r.oversize, 0)} of ${answers} |`,
    `| Clef latency per chunk (median / max, s) | ${mm(ms, s1)} |`,
    `| decided rate, chunk answers >= ${ACT} or <= ${(1 - ACT).toFixed(1)} | ${pct(decided, answers)} |`,
    `| artifacts whose check is decided by the fold | ${pct(runs.filter((r) => r.folded.value !== undefined).length, runs.length)} |`,
    `| Clef time per artifact (median / max, s) | ${mm(runs.map((r) => clefMs(r) / 1000), (x) => x.toFixed(1))} |`,
    `| Clef plus 27B for what stays undecided (median / max, s) | ${mm(runs.map(withEscalation), (x) => x.toFixed(1))} |`,
    ...(today ? [`| today, whole artifact (median / max, s) | ${today} |`] : []),
  ].join('\n');
}

const head = '| measure | value |\n| --- | --- |';

// Research against the labels with measure.ts's flag rule: a positive check is flagged below 1, a pitfall above 0.
const cases = new Map(researchCases().map((c) => [c.case, c]));
const research: CheckRun[] = load('research');
const baseline = load('baseline') as Array<{ case: string; asks: any; review: any }>;
const bool = (r: any, id: string) => r?.answers?.[id]?.p as number | undefined;

function labels(check: string, flagWhen: boolean, flagOf: (c: string) => { flagged?: boolean; guess: boolean } | undefined) {
  const rows = [...cases.values()].map((c) => ({ c, f: flagOf(c.case) })).filter((x) => x.f);
  const bad = rows.filter((x) => x.c.targets.includes(check as never));
  const good = rows.filter((x) => x.c.good);
  const g = (xs: typeof rows, p: (x: (typeof rows)[number]) => boolean) => xs.filter(p).length;
  const decidedOf = (xs: typeof rows) => xs.filter((x) => x.f!.flagged !== undefined);
  return `catch on seeded-bad ${pct(g(bad, (x) => x.f!.guess), bad.length)}, false block on good ${pct(g(good, (x) => x.f!.guess), good.length)}; decided only: catch ${pct(g(decidedOf(bad), (x) => !!x.f!.flagged), decidedOf(bad).length)}, false block ${pct(g(decidedOf(good), (x) => !!x.f!.flagged), decidedOf(good).length)}`;
}

const out: string[] = [];
for (const [check, flagWhen, baseOf] of [['answers_every_ask', false, (b: any) => bool(b.asks, 'answers_every_ask')], ['summary_follows_findings', false, (b: any) => bool(b.review, 'summary_follows_findings')], ['no_self_contradiction', true, (b: any) => bool(b.review, 'no_self_contradiction')]] as const) {
  const runs = research.filter((r) => r.check === check);
  const base = baseline.map((b) => ({ b, p: baseOf(b) }));
  const baseMs = baseline.map((b) => (check === 'answers_every_ask' ? b.asks.ms : b.review.ms));
  const todayTimes = base.map(({ p }, i) => baseMs[i] / 1000 + (p === undefined || (p < ACT && 1 - p < ACT) ? ESCALATION_S : 0));
  const baseDecided = base.filter(({ p }) => p !== undefined && (p >= ACT || 1 - p >= ACT)).length;
  const flagFold = (c: string) => { const r = runs.find((x) => x.artifact === c)!; return { flagged: r.folded.value === undefined ? undefined : r.folded.value === flagWhen, guess: r.folded.guess === flagWhen }; };
  const flagBase = (c: string) => { const p = baseOf(baseline.find((b) => b.case === c)!); return p === undefined ? undefined : { flagged: p >= ACT || 1 - p >= ACT ? (p >= 0.5) === flagWhen : undefined, guess: (p >= 0.5) === flagWhen }; };
  out.push(`### ${check}\n\n${head}\n${shape(runs, `${mm(todayTimes, (x) => x.toFixed(1))}; whole-question Clef answer decided on ${pct(baseDecided, base.length)} of cases`)}\n| labels, fan-out | ${labels(check, flagWhen, flagFold)} |\n| labels, whole question to Clef | ${labels(check, flagWhen, flagBase)} |`);
}

// Real research reports.
const prod: CheckRun[] = load('prod-stats');
out.push(`### Real reports (${new Set(prod.map((r) => r.artifact)).size} on prod; Clef run on four, chunk shape on all)\n\n| check | reports | chunks per report (median / max) | tokens per chunk (median / max) | oversize | decided rate (the four run) |\n| --- | --- | --- | --- | --- | --- |\n${['answers_every_ask', 'summary_follows_findings', 'no_self_contradiction'].map((c) => {
  const rs = prod.filter((r) => r.check === c);
  const ran = rs.filter((r) => sum(r.ms) > 0);
  return `| ${c} | ${rs.length} | ${mm(rs.map((r) => r.chunks))} | ${mm(rs.flatMap((r) => r.tokens))} | ${sum(rs.map((r) => r.oversize))} | ${pct(sum(ran.map((r) => r.decided.filter(Boolean).length)), sum(ran.map((r) => r.chunks)))} |`;
}).join('\n')}`);

// Reviews.
const reviews = load('review') as Array<{ n: number; wholeTokens: number; recorded: Record<string, { met: number | null; resolvers: Record<string, number>; notes: string[] }>; checks: CheckRun[] }>;
const rc = (id: string) => reviews.map((r) => ({ r, c: r.checks.find((c) => c.check === id)! })).filter((x) => x.c);
const escalated = (r: (typeof reviews)[number], id: string) => Object.keys(r.recorded[id]?.resolvers ?? {}).some((k) => k !== 'clef' && k !== 'code');
const todayReview = (id: string) => rc(id).map(({ r }) => 0.4 + (escalated(r, id) ? ESCALATION_S : 0));

for (const [id, rec] of [['promises_handled', 'promises_handled'], ['sibling_parts', 'sibling_parts']] as const) {
  const xs = rc(id).filter((x) => id !== 'sibling_parts' || x.c.chunks > 0);
  const runs = xs.map((x) => x.c);
  const agree = xs.filter((x) => x.r.recorded[rec]?.met !== null && x.r.recorded[rec] && escalated(x.r, rec));
  // recorded met is 1 when the 27B found nothing missing; the fold's guess is true when every chunk passes (promises) or false when no sibling gap is found.
  const ok = (x: (typeof xs)[number]) => (id === 'promises_handled' ? x.c.folded.guess : !x.c.folded.guess) === (x.r.recorded[rec].met === 1);
  const dec = agree.filter((x) => x.c.folded.value !== undefined);
  out.push(`### ${id}${id === 'sibling_parts' ? ` (triggered on ${xs.length} of ${reviews.length} reviews)` : ''}\n\n${head}\n${shape(runs, `${mm(todayReview(id).slice(0, xs.length), (x) => x.toFixed(1))}; recorded: escalated to the 27B on ${reviews.filter((r) => escalated(r, rec)).length} of ${reviews.length}`)}\n| agreement with the recorded 27B answer | ${pct(agree.filter(ok).length, agree.length)}; where the fold decided: ${pct(dec.filter(ok).length, dec.length)} |`);
}

const files = rc('covered');
const q1 = files.flatMap((x) => x.c.decided);
const unnamed = rc('unnamed_defect');
const codeCovered = files.reduce((a, x) => a + x.c.related.filter((n) => n > 0).length, 0);
const nsa = unnamed.filter((x) => x.r.recorded.no_silent_approval);
const nsaOk = nsa.filter((x) => x.c.folded.guess === (x.r.recorded.no_silent_approval.met === 0));
out.push(`### Per-file review coverage (measurement only)\n\n${head}\n${shape(files.map((x) => x.c))}\n| files with review lines found by code (covered without the model) | ${pct(codeCovered, q1.length)} |\n| decided rate, Q1 "does the review cover this file" | ${pct(q1.filter(Boolean).length, q1.length)} |\n| decided rate, Q2 "defect the review does not name" | ${pct(unnamed.flatMap((x) => x.c.decided).filter(Boolean).length, unnamed.flatMap((x) => x.c.decided).length)} |\n| reviews with every file covered (fold true / undecided / false) | ${files.filter((x) => x.c.folded.value === true).length} / ${files.filter((x) => x.c.folded.value === undefined).length} / ${files.filter((x) => x.c.folded.value === false).length} of ${files.length} |\n| reviews where Q2 finds an unnamed defect in some file (best guess) | ${pct(unnamed.filter((x) => x.c.folded.guess).length, unnamed.length)} |\n| reviews where the 27B recorded an unmentioned defect (no_silent_approval = 0) | ${pct(nsa.filter((x) => x.r.recorded.no_silent_approval.met === 0).length, nsa.length)} |\n| agreement of Q2 with the recorded no_silent_approval | ${pct(nsaOk.length, nsa.length)} |`);

// Contracts.
const specs = load('contracts') as Array<{ key: string; kind: string; recorded: Record<string, { met: number | null }>; checks: CheckRun[] }>;
for (const id of ['meets_intent', 'follows_guide', 'adds_unasked']) {
  const xs = specs.map((s) => ({ s, c: s.checks.find((c) => c.check === id)! }));
  // adds_unasked is a pitfall: recorded met 0 means nothing unasked; the fold passes when every part was asked for.
  const ok = xs.filter((x) => x.c.folded.guess === (id === 'adds_unasked' ? x.s.recorded[id].met === 0 : x.s.recorded[id].met === 1));
  out.push(`### ${id}\n\n${head}\n${shape(xs.map((x) => x.c), `${(0.4 + ESCALATION_S).toFixed(1)}; the 27B answered all ${specs.length} recorded verdicts`)}\n| agreement with the recorded 27B answer (all four specs passed) | ${pct(ok.length, xs.length)}; decided by the fold: ${pct(xs.filter((x) => x.c.folded.value !== undefined).length, xs.length)} |`);
}
// Splitters: the same inputs under each way of splitting, against hand-labelled gold items.
const plain = (s: string) => s.toLowerCase().replace(/[`*]/g, '').replace(/\s+/g, ' ').trim();
const variantRuns: CheckRun[] = [...load('splitters'), ...load('splitters-hybrid').filter((r: CheckRun) => /Hybrid$/.test(r.check))];
const goldAsks = (artifact: string): string[] | undefined => {
  if (artifact.startsWith('prod-')) return ASKS.find((a) => a.has === (artifact.includes('18133686') ? 'parallel implementations' : 'tautological or vacuous'))?.items.map((i) => i.span);
  const c = cases.get(artifact);
  const req = c ? (c.evidence.request![0].state as { request: string }).request : '';
  return ASKS.find((a) => req.includes(a.has))?.items.map((i) => i.span);
};
function splitTable(label: string, prefix: string, groups: Array<[string, (r: CheckRun) => boolean]>, gold: (a: string) => string[] | undefined) {
  const rows = ['| splitter | artifacts | items (median / max) | gold found | gold isolated in its own item | items with no gold | Clef calls per artifact (median / max) | fallback ran |', '| --- | --- | --- | --- | --- | --- | --- | --- |'];
  for (const [name, pick] of groups) {
    const runs = variantRuns.filter((r) => r.check.startsWith(prefix) && pick(r) && gold(r.artifact));
    let found = 0, isolated = 0, total = 0, junk = 0, items = 0;
    for (const r of runs) {
      const g = gold(r.artifact)!.map(plain);
      const texts = r.texts.map(plain);
      total += g.length;
      items += texts.length;
      found += g.filter((x) => texts.some((t) => t.includes(x))).length;
      isolated += g.filter((x) => texts.some((t) => t.includes(x) && g.filter((y) => t.includes(y)).length === 1)).length;
      junk += texts.filter((t) => !g.some((x) => t.includes(x) || x.includes(t))).length;
    }
    const fell = runs.filter((r) => r.splitter.includes('->')).map((r) => `${r.artifact} (${r.reason})`);
    rows.push(`| ${name} | ${runs.length} | ${mm(runs.map((r) => r.chunks))} | ${pct(found, total)} | ${pct(isolated, total)} | ${pct(junk, items)} | ${mm(runs.map((r) => r.chunks))} | ${fell.length ? fell.join('; ') : 'no'} |`);
  }
  return `### Splitters on ${label}\n\n${rows.join('\n')}`;
}
const names = (p: string, d: [string, string][]): Array<[string, (r: CheckRun) => boolean]> => d.map(([label, v]) => [label, (r: CheckRun) => r.check.endsWith(`/${p}${v}`)]);
const only = (f: (a: string) => boolean) => (a: string) => (f(a) ? goldAsks(a) : undefined);
const askNames = names('asks', [['deterministic (sentences + clause split)', 'Det'], ['deterministic with model fallback', 'Auto'], ['model (simulated)', 'Llm'], ['model + sweep of leftover text', 'Hybrid']]);
out.push(splitTable('research request asks, the 20 labelled cases', 'answers_every_ask', askNames, only((a) => cases.has(a))));
out.push(splitTable('research request asks, two real prod requests (free-form, with a context paragraph)', 'answers_every_ask', askNames, only((a) => a.startsWith('prod-'))));
const goldPromises = (a: string) => (PROMISES[Number(a.slice(1))] ? PROMISES[Number(a.slice(1))].map((p) => p.span) : undefined);
const promiseNames = names('promises', [['deterministic (sentences + delivery-verb filter, cap 5)', 'Det'], ['deterministic with model fallback (no cap; > 6 items falls back)', 'Auto'], ['model (simulated)', 'Llm'], ['model + sweep of leftover text', 'Hybrid']]);
out.push(splitTable('PR description promises, 5 reviews', 'promises_handled', promiseNames, goldPromises));
// What each variant says about the check itself, per artifact.
const verdicts = variantRuns.filter((r) => /^(answers_every_ask|promises_handled)\//.test(r.check) && (r.artifact.startsWith('prod-') || r.artifact.startsWith('#')));
out.push(`### Check outcome by splitter (best guess; ? = undecided)\n\n| artifact | check | splitter | items | folded value | swept items | rejected spans |\n| --- | --- | --- | --- | --- | --- | --- |\n${verdicts.map((r) => `| ${r.artifact} | ${r.check} | ${r.splitter} | ${r.chunks} | ${r.folded.value === undefined ? '?' : r.folded.value} (guess ${r.folded.guess}) | ${r.swept} | ${r.rejected} |`).join('\n')}`);
console.log(out.join('\n\n'));

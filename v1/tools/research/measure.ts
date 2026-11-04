// Judges the labelled research set under the current research.report rubric against a local OpenAI-compatible model and prints per-check catch, false-block and swing.
// Usage: node tools/research/measure.ts <base-url> <model-id> <metrics-url> [repeat] [rows.jsonl]
import { appendFileSync, existsSync, readFileSync } from 'node:fs';
import { buildModels, parseDeployment } from '@quack/session';
import { researchRubric } from '@quack/research';
import { score } from '@quack/judge';
import { replayJudge, type Row } from '@quack/eval';
import { researchCases, type Labelled } from './cases.ts';

// The audit's weights; flagging and the planned-rubric block rate use these, whatever weights the rubric ships with.
const PLANNED: Record<string, number> = { supported_by_source: 4, contradicted_by_source: -4, answers_every_ask: 4, summary_follows_findings: 3, no_self_contradiction: -4 };
const BUSY_MAX = 2;
const POLL_MS = 15_000;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Waits until the shared GPU has at most BUSY_MAX requests running, so a measurement never starves prod. */
async function whenIdle(metricsUrl: string): Promise<void> {
  for (;;) {
    const text = await (await fetch(metricsUrl)).text();
    const running = Number(/^vllm:num_requests_running\{[^}]*\}\s+(\S+)/m.exec(text)?.[1] ?? Infinity);
    if (running <= BUSY_MAX) return;
    await sleep(POLL_MS);
  }
}

const flagged = (weight: number, met: number) => (weight > 0 ? met < 1 : met > 0);
const pct = (n: number, d: number) => (d ? `${Math.round((100 * n) / d)}% (${n}/${d})` : 'n/a');

export function summarize(rows: Row[], cases: Labelled[]): string {
  const shipped = new Map(researchRubric.criteria.flatMap((c) => c.checks.map((k) => [k.id, k.weight] as const)));
  const by = new Map(cases.map((c) => [c.case, c]));
  const checks = [...new Set(rows.map((r) => r.check))].filter((id) => id in PLANNED);
  const lines = ['| check | catch on seeded-bad | false-block on good | undecided | cases whose reps disagree | mean swing |', '| --- | --- | --- | --- | --- | --- |'];
  for (const check of checks) {
    const w = PLANNED[check];
    const mine = rows.filter((r) => r.check === check);
    const decided = mine.filter((r) => r.met !== null);
    const hit = (r: Row) => flagged(w, r.met!);
    const bad = decided.filter((r) => by.get(r.case)!.targets.includes(check as never));
    const good = decided.filter((r) => by.get(r.case)!.good);
    const swings = [...new Set(decided.map((r) => r.case))].map((c) => decided.filter((r) => r.case === c).map((r) => r.met!)).map((m) => Math.max(...m) - Math.min(...m));
    const split = [...new Set(decided.map((r) => r.case))].filter((c) => new Set(decided.filter((r) => r.case === c).map(hit)).size > 1).length;
    lines.push(`| ${check} | ${pct(bad.filter(hit).length, bad.length)} | ${pct(good.filter(hit).length, good.length)} | ${mine.length - decided.length} | ${split} of ${swings.length} | ${(swings.reduce((a, b) => a + b, 0) / (swings.length || 1)).toFixed(2)} |`);
  }
  const blocked = (want: boolean, weight: (id: string) => number | 'gate') => {
    const runs = new Map<string, Row[]>();
    for (const r of rows.filter((x) => by.get(x.case)!.good === want)) runs.set(`${r.rep}\t${r.case}`, [...(runs.get(`${r.rep}\t${r.case}`) ?? []), r]);
    const verdicts = [...runs.values()].map((rs) => score(researchRubric, rs.map((r) => ({ criterion: '', id: r.check, label: '', weight: weight(r.check), met: r.met, units: 1, resolvers: {}, notes: [] }))));
    return pct(verdicts.filter((v) => !v.passed).length, verdicts.length);
  };
  const planned = (id: string) => PLANNED[id] ?? 'gate';
  const ships = (id: string) => shipped.get(id)!;
  const line = (label: string, w: (id: string) => number | 'gate') => `${label}: good ${blocked(true, w)}, seeded-bad ${blocked(false, w)}`;
  return [...lines, '', `whole-report block rate, score under ${researchRubric.threshold} or a gate failed`, line('  audit weights (all five scored)', planned), line('  shipped weights', ships)].join('\n');
}

if (import.meta.main) {
  const [baseUrl, modelId, metricsUrl, repeatArg, out = 'rows.jsonl'] = process.argv.slice(2);
  if (!baseUrl || !modelId || !metricsUrl) throw new Error('usage: measure.ts <base-url> <model-id> <metrics-url> [repeat] [rows.jsonl]');
  const repeat = Number(repeatArg ?? 3);
  const cases = researchCases();
  const deployment = parseDeployment({ providers: { local: { baseUrl, models: { [modelId]: { contextWindow: 262144, maxTokens: 16384 } } } } });
  const rows: Row[] = existsSync(out) ? readFileSync(out, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l)) : [];
  const done = new Set(rows.map((r) => `${r.rep}\t${r.case}`));
  const adapters = [{ kind: 'research.report', judge: { rubric: researchRubric, evidence: async () => undefined } }];
  // One case at a time, one repeat round after another, so the GPU gate applies before every case and drift spreads across cases.
  for (let rep = 1; rep <= repeat; rep++) {
    for (const c of cases) {
      if (done.has(`${rep}\t${c.case}`)) continue;
      await whenIdle(metricsUrl);
      const got = await replayJudge({ cases: [c], arms: [{ label: 'v2', adapters }], repeat: 1, concurrency: 1, models: buildModels(deployment), judgeModel: { provider: 'local', modelId } });
      for (const r of got) {
        appendFileSync(out, `${JSON.stringify({ ...r, rep })}\n`);
        rows.push({ ...r, rep });
      }
      console.error(`rep ${rep} ${c.case}: ${got.map((r) => `${r.check}=${r.met}`).join(' ')}`);
    }
  }
  console.log(summarize(rows, cases));
}

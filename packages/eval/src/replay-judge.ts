// Judges recorded cases under one or more arms, N times, and prints the met table; an arm is label=ref[:decision-model][:reuse] (docs/README.md).
// Usage: see `usage` below; `--table <rows.jsonl>` prints the table of a rows file.
import { randomInt } from 'node:crypto';
import { appendFileSync, readFileSync } from 'node:fs';
import { parseArgs } from 'node:util';
import { buildModels, parseDeployment, resolveDeployment } from '@quack/session';
import { armAt, budget, replayJudge, table, type Arm, type Expect, type Row } from './judge-replay.ts';

const usage = 'usage: replay-judge.ts --cases <cases.json> --deployment <deployment.json> --judge <provider/model> [--decision-model <provider/model>] [--arm label=ref[:none|:provider/model][:reuse]]... [--repeat N] [--concurrency N] [--seed N] [--rows <rows.jsonl>] [--report-checks <id>...] [--decision-only [--expect <labels.json>]] [--budget] | --table <rows.jsonl> [--report-checks <id>...] [--expect <labels.json>] [--budget]';
const { values } = parseArgs({ options: { cases: { type: 'string' }, deployment: { type: 'string' }, judge: { type: 'string' }, 'decision-model': { type: 'string' }, arm: { type: 'string', multiple: true }, repeat: { type: 'string', default: '3' }, concurrency: { type: 'string', default: '1' }, seed: { type: 'string' }, rows: { type: 'string' }, table: { type: 'string' }, 'report-checks': { type: 'string', multiple: true }, 'decision-only': { type: 'boolean' }, expect: { type: 'string' }, budget: { type: 'boolean' } } });
const reportChecks = values['report-checks']?.flatMap((s) => s.split(',').filter(Boolean));
// Labels per item: { case: { unit id: { flag } } }; only decision-only rows carry the item outcomes they are read against.
const expect = values.expect ? (JSON.parse(readFileSync(values.expect, 'utf8')) as Expect) : undefined;
if (expect && !values.table && !values['decision-only']) throw new Error('--expect needs --decision-only or --table');

// --budget: each arm's checks by escalation rate after the table; a check over budget fails the command.
function printBudget(rows: Row[]): number {
  if (!values.budget) return 0;
  const { lines, unhealthy } = budget(rows.map((r) => ({ key: `${r.arm} ${r.rubric ?? '?'} ${r.criterion ?? '?'}.${r.check}`, clef: r.clef, esc: r.esc, route: r.route })));
  console.log(['', 'escalation budget (escalated / asked per arm, rubric and check):', ...lines].join('\n'));
  return unhealthy ? 1 : 0;
}

// A rows file, from a finished run or one that was stopped, prints as the table it would have printed; the seed is not in it.
if (values.table) {
  const rows = readFileSync(values.table, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l) as Row);
  console.log(table({ rows, declined: [] }, [...new Set(rows.map((r) => r.arm))], reportChecks, expect));
  process.exitCode = printBudget(rows);
} else {
  await run();
}

async function run() {
  if (!values.cases || !values.deployment || !values.judge?.includes('/')) throw new Error(usage);
  const repeat = Number(values.repeat);
  const concurrency = Number(values.concurrency);
  const seed = values.seed === undefined ? randomInt(2 ** 31) : Number(values.seed);
  if (![repeat, concurrency].every((n) => Number.isInteger(n) && n >= 1) || !Number.isInteger(seed)) throw new Error('--repeat and --concurrency must be positive integers, and --seed an integer');

  const deployment = await resolveDeployment(parseDeployment(JSON.parse(readFileSync(values.deployment, 'utf8'))));
  const ref = (s: string) => ({ provider: s.slice(0, s.indexOf('/')), modelId: s.slice(s.indexOf('/') + 1) });
  const decisionModelOf = (s: string) => {
    if (!s.includes('/')) throw new Error(`decision model "${s}" is neither provider/model nor none`);
    const r = ref(s);
    return { ...r, maxInputTokens: deployment.providers[r.provider]?.models?.[r.modelId]?.contextWindow ?? 8192 };
  };
  const arms: Arm[] = [];
  // A ref cannot contain ":", so what follows one names the arm's decision model: "none" or provider/model; absent, --decision-model's.
  for (const spec of values.arm ?? ['current=.']) {
    const [label, rest = '.'] = spec.split('=');
    const parts = rest.split(':');
    // A trailing ":reuse" judges each recorded session's cases in version order with item reuse on.
    const reuse = parts.length > 1 && parts.at(-1) === 'reuse';
    if (reuse) parts.pop();
    const [at, cls = values['decision-model'] ?? 'none'] = parts;
    arms.push({ label, ...(await armAt(at)), decisionModel: cls === 'none' ? undefined : decisionModelOf(cls), ...(reuse && { reuse }) });
  }
  // Printed first, so a run that is stopped can still be replayed in the same arm order.
  console.error(`arm order seed ${seed}`);
  const file = values.rows;
  const onRows = file ? (rows: Row[]) => appendFileSync(file, rows.map((r) => `${JSON.stringify(r)}\n`).join('')) : undefined;
  const result = await replayJudge({ cases: JSON.parse(readFileSync(values.cases, 'utf8')), arms, repeat, concurrency, models: buildModels(deployment), judgeModel: ref(values.judge), seed, onRows, decisionOnly: values['decision-only'] });
  console.log(table(result, arms.map((a) => a.label), reportChecks, expect));
  process.exitCode = printBudget(result.rows);
}

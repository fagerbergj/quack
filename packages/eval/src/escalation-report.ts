// Each rubric check's escalation rate over the verdicts in a directory of catalog exports (`quack export <key> <dir>/<name>.json`):
// escalated / (decision model + escalated) per rubric@version and check, over budget marked UNHEALTHY, which fails the command.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parseArgs } from 'node:util';
import type { Verdict } from '@quack/judge';
import { budget, escalated } from './judge-replay.ts';

const USAGE = 'usage: escalation-report.ts <exports-dir> [--decision-model <modelId>] [--since <epoch ms>]';

type Exported = { artifacts: Array<{ sealedAt: number; verdict?: Verdict | null }> };

/** One count per check of every verdict sealed at or after `since`: items the decision model settled, and those a model besides it escalated to. */
export function countsOf(exports: Exported[], decisionModel: string, since = 0) {
  const verdicts = exports.flatMap((e) => e.artifacts).filter((a) => a.verdict && a.sealedAt >= since).map((a) => a.verdict!);
  return verdicts.flatMap((v) => v.checks.map((c) => ({ key: `${v.rubric} ${c.criterion}.${c.id}`, clef: c.resolvers[decisionModel] ?? 0, esc: escalated(c.resolvers, decisionModel) })));
}

if (import.meta.main) {
  const { values, positionals } = parseArgs({ allowPositionals: true, options: { 'decision-model': { type: 'string', default: 'clef' }, since: { type: 'string', default: '0' } } });
  const [dir] = positionals;
  if (!dir || !Number.isInteger(Number(values.since))) throw new Error(USAGE);
  const exports = readdirSync(dir).filter((f) => f.endsWith('.json')).map((f) => JSON.parse(readFileSync(join(dir, f), 'utf8')) as Exported);
  const { lines, unhealthy } = budget(countsOf(exports, values['decision-model'], Number(values.since)));
  console.log(['escalation budget (escalated / asked per rubric and check):', ...lines].join('\n'));
  process.exitCode = unhealthy ? 1 : 0;
}

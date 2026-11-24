// Each rubric check's escalation rate over the verdicts in a directory of catalog exports (`quack export <key> <dir>/<name>.json`):
// escalated / (decision model + escalated) per rubric@version and check, with the causes a verdict records; over budget is UNHEALTHY, and an old verdict's check Clef never settled is no-clef, listed apart.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parseArgs } from 'node:util';
import type { Verdict } from '@quack/judge';
import { budget, escalated } from './judge-replay.ts';

const CAUSELESS = ['outage'];

const USAGE = 'usage: escalation-report.ts <exports-dir> [--decision-model <modelId>] [--since <epoch ms>]';

type Exported = { artifacts: Array<{ sealedAt: number; verdict?: Verdict | null }> };

/** One count per check of every judge verdict sealed at or after `since`: items the decision model settled, and those a model besides it escalated to. */
export function countsOf(exports: Exported[], decisionModel: string, since = 0) {
  const verdicts = exports.flatMap((e) => e.artifacts).filter((a) => Array.isArray(a.verdict?.checks) && a.sealedAt >= since).map((a) => a.verdict!);
  return verdicts.flatMap((v) => v.checks.map((c) => {
    const key = `${v.rubric} ${c.criterion}.${c.id}`;
    const clef = c.resolvers[decisionModel] ?? 0;
    if (!c.escalated) return { key, clef, esc: escalated(c.resolvers, decisionModel) };
    // An outage leaves its item undecided, so it is shown but not rated as an escalation.
    const causes = Object.entries(c.escalated).filter(([k]) => !CAUSELESS.includes(k)).map(([, n]) => n);
    const declared = Object.keys(c.escalated).every((k) => k === 'declared');
    return { key, clef, esc: causes.reduce((a, b) => a + b, 0), causes: c.escalated, ...(declared && { route: 'declared' as const }) };
  }));
}

// A file that is not a quack export costs only itself.
function readExport(dir: string, f: string): Exported[] {
  try {
    const e = JSON.parse(readFileSync(join(dir, f), 'utf8')) as Exported;
    if (Array.isArray(e?.artifacts)) return [e];
  } catch {
    // Unreadable or not JSON: reported below like any other skip.
  }
  console.error(`skipped ${f}: not a quack export`);
  return [];
}

/** The split of rated lines by mark, so no reader counts them by hand. */
export function tally(lines: string[]): string {
  const n = (m: string) => lines.filter((l) => l.startsWith(m)).length;
  return `${n('UNHEALTHY')} UNHEALTHY, ${n('no-clef')} no-clef, ${n('ok')} ok, ${n('declared')} declared, ${n('not-asked')} not-asked`;
}

if (import.meta.main) {
  const { values, positionals } = parseArgs({ allowPositionals: true, options: { 'decision-model': { type: 'string', default: 'clef' }, since: { type: 'string', default: '0' } } });
  const [dir] = positionals;
  if (!dir || !Number.isInteger(Number(values.since))) throw new Error(USAGE);
  const exports = readdirSync(dir).filter((f) => f.endsWith('.json')).flatMap((f) => readExport(dir, f));
  const counts = countsOf(exports, values['decision-model'], Number(values.since));
  const { lines, unhealthy } = budget(counts);
  console.log([`escalation budget from ${exports.length} exports, ${counts.length} verdict checks sealed since ${values.since} (escalated / asked per rubric and check):`, ...lines, tally(lines)].join('\n'));
  // Reading nothing is a failure, not a clean bill.
  process.exitCode = unhealthy || !counts.length ? 1 : 0;
}

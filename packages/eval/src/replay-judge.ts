// Judges recorded cases under one or more arms, N times, and prints the met table; an arm is label=ref[:decision-model] (docs/README.md).
// Usage: node packages/eval/src/replay-judge.ts --cases <cases.json> --deployment <deployment.json> --judge <provider/model> [--decision-model <provider/model>] [--arm label=ref[:none|:provider/model]]... [--repeat N] [--concurrency N] [--escalations N]
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import { extensionList, loadManifests } from '@quack/runtime/catalog';
import { buildModels, parseDeployment, resolveDeployment } from '@quack/session';
import { adaptersOf } from './adapters.ts';
import { packagesTar, replayJudge, table, type Arm } from './judge-replay.ts';

/** The adapters of the extensions as they stood at ref: their tree is unpacked beside this one's node_modules and imported from there. */
async function adaptersAt(ref: string) {
  if (ref === '.') return adaptersOf(await loadManifests());
  const dir = mkdtempSync(join(tmpdir(), 'quack-arm-'));
  try {
    const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
    execFileSync('tar', ['-x', '-C', dir], { input: packagesTar(top, ref) });
    symlinkSync(fileURLToPath(new URL('../../../node_modules', import.meta.url)), join(dir, 'node_modules'));
    // A package the ref leaves as it is here stays this tree's module: a second copy would register its splitters twice.
    const changed = (pkg: string) => spawnSync('git', ['diff', '--quiet', ref, '--', `packages/${pkg}`], { cwd: top }).status !== 0;
    const specs = extensionList().filter((s) => s.startsWith('@quack/')).map((s) => (changed(s.slice(7)) ? pathToFileURL(join(dir, 'packages', s.slice(7), 'src/index.ts')).href : s));
    return adaptersOf(await loadManifests(specs));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

const usage = 'usage: replay-judge.ts --cases <cases.json> --deployment <deployment.json> --judge <provider/model> [--decision-model <provider/model>] [--arm label=ref[:none|:provider/model]]... [--repeat N] [--concurrency N] [--escalations N]';
const { values } = parseArgs({ options: { cases: { type: 'string' }, deployment: { type: 'string' }, judge: { type: 'string' }, 'decision-model': { type: 'string' }, arm: { type: 'string', multiple: true }, repeat: { type: 'string', default: '3' }, concurrency: { type: 'string', default: '1' }, escalations: { type: 'string', default: '4' } } });
if (!values.cases || !values.deployment || !values.judge?.includes('/')) throw new Error(usage);
const repeat = Number(values.repeat);
const concurrency = Number(values.concurrency);
const escalations = Number(values.escalations);
if (![repeat, concurrency, escalations].every((n) => Number.isInteger(n) && n >= 1)) throw new Error('--repeat, --concurrency and --escalations must be positive integers');

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
  const [at, cls = values['decision-model'] ?? 'none'] = rest.split(':');
  arms.push({ label, adapters: await adaptersAt(at), decisionModel: cls === 'none' ? undefined : decisionModelOf(cls) });
}
const result = await replayJudge({ cases: JSON.parse(readFileSync(values.cases, 'utf8')), arms, repeat, concurrency, models: buildModels(deployment), judgeModel: ref(values.judge), escalations });
console.log(table(result, arms.map((a) => a.label)));

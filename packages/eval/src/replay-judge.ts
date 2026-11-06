// Judges recorded cases under one or more rubric arms, N times, and prints the met table; an arm's ref is "." or a git ref (docs/v1/README.md).
// Usage: node packages/eval/src/replay-judge.ts --cases <cases.json> --deployment <deployment.json> --judge <provider/model> [--classifier <provider/model>] [--arm label=ref]... [--repeat N] [--concurrency N]
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import { extensionList, loadManifests } from '@quack/runtime/catalog';
import { buildModels, parseDeployment, resolveDeployment } from '@quack/session';
import { adaptersOf } from './adapters.ts';
import { replayJudge, table, type Arm } from './judge-replay.ts';

/** The adapters of the extensions as they stood at ref: their tree is unpacked beside this one's node_modules and imported from there. */
async function adaptersAt(ref: string) {
  if (ref === '.') return adaptersOf(await loadManifests());
  const dir = mkdtempSync(join(tmpdir(), 'quack-arm-'));
  try {
    const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
    const tar = execFileSync('git', ['archive', '--format=tar', `${ref}:v1`, 'packages'], { cwd: top, maxBuffer: 1 << 30 });
    execFileSync('tar', ['-x', '-C', dir], { input: tar });
    symlinkSync(fileURLToPath(new URL('../../../node_modules', import.meta.url)), join(dir, 'node_modules'));
    const specs = extensionList().filter((s) => s.startsWith('@quack/')).map((s) => pathToFileURL(join(dir, 'packages', s.slice(7), 'src/index.ts')).href);
    return adaptersOf(await loadManifests(specs));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

const usage = 'usage: replay-judge.ts --cases <cases.json> --deployment <deployment.json> --judge <provider/model> [--classifier <provider/model>] [--arm label=ref]... [--repeat N] [--concurrency N]';
const { values } = parseArgs({ options: { cases: { type: 'string' }, deployment: { type: 'string' }, judge: { type: 'string' }, classifier: { type: 'string' }, arm: { type: 'string', multiple: true }, repeat: { type: 'string', default: '3' }, concurrency: { type: 'string', default: '1' } } });
if (!values.cases || !values.deployment || !values.judge?.includes('/')) throw new Error(usage);
const repeat = Number(values.repeat);
const concurrency = Number(values.concurrency);
if (!Number.isInteger(repeat) || repeat < 1 || !Number.isInteger(concurrency) || concurrency < 1) throw new Error('--repeat and --concurrency must be positive integers');

const deployment = await resolveDeployment(parseDeployment(JSON.parse(readFileSync(values.deployment, 'utf8'))));
const ref = (s: string) => ({ provider: s.slice(0, s.indexOf('/')), modelId: s.slice(s.indexOf('/') + 1) });
const classifierRef = values.classifier ? ref(values.classifier) : undefined;
const classifier = classifierRef && { ...classifierRef, maxInputTokens: deployment.providers[classifierRef.provider]?.models?.[classifierRef.modelId]?.contextWindow ?? 8192 };
const arms: Arm[] = [];
for (const spec of values.arm ?? ['current=.']) {
  const [label, at] = spec.split('=');
  arms.push({ label, adapters: await adaptersAt(at ?? '.') });
}
const rows = await replayJudge({ cases: JSON.parse(readFileSync(values.cases, 'utf8')), arms, repeat, concurrency, models: buildModels(deployment), judgeModel: ref(values.judge), classifier });
console.log(table(rows, arms.map((a) => a.label)));

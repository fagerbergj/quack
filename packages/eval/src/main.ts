// Usage: main.ts dataset <dataset> <agent> <good|bad|-> <session-dir>...
//        main.ts run [--concurrency=N] <dataset> <run-name> <deployment.json> <agent.json> [item-id...]
import { resolve } from 'node:path';
import { loadManifests } from '@quack/runtime/catalog';
import { adaptersOf, blocksOf } from './adapters.ts';
import { itemFrom, type Label } from './dataset.ts';
import { langfuse } from './langfuse.ts';
import { readSession } from './record.ts';
import { replay } from './replay.ts';
import { eachItem } from './pool.ts';
import { publish, scoresOf, summarize } from './report.ts';

const adapters = adaptersOf(await loadManifests());
const blocks = blocksOf(adapters);
const MIN_CASES = 20;

function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const lf = langfuse({ baseUrl: env('LANGFUSE_BASE_URL'), publicKey: env('LANGFUSE_PUBLIC_KEY'), secretKey: env('LANGFUSE_SECRET_KEY') });
const args = process.argv.slice(2);
// vLLM serves several sequences at once; leave slots free for the deployment's live sessions.
const concurrency = Number(args.find((a) => a.startsWith('--concurrency='))?.split('=')[1] ?? 1);
if (!Number.isInteger(concurrency) || concurrency < 1) throw new Error(`--concurrency must be a positive integer, got ${concurrency}`);
const [command, dataset, ...rest] = args.filter((a) => !a.startsWith('--'));

if (command === 'dataset' && dataset && rest.length >= 3) {
  const [agent, label, ...dirs] = rest;
  await lf.ensureDataset(dataset);
  for (const dir of dirs) {
    const record = await readSession(dir);
    // A wrong path reads as an empty session; it must not become a benchmark case.
    if (!record.inputs.length) throw new Error(`${dir} has no recorded session`);
    const item = itemFrom(record, { agent, label: label === '-' ? undefined : (label as Label) });
    await lf.upsertItem(dataset, item);
    console.log(`${item.id} ${label} ${dir}`);
  }
} else if (command === 'run' && dataset && rest.length >= 3) {
  const [runName, deployment, agent, ...only] = rest;
  const items = (await lf.items(dataset)).filter((i) => !only.length || only.includes(i.id));
  // A benchmark that can promote an agent needs 20 real cases; a partial run still reports, it just cannot promote.
  if (!only.length && items.length < MIN_CASES) console.log(`note: ${dataset} has ${items.length} items; a promotion needs ${MIN_CASES}`);
  const { results: rows, failures } = await eachItem(items, concurrency, async (item) => {
    const r = await replay(item, { deployment: resolve(deployment), agent: resolve(agent) }, adapters);
    const extra = r.codeScores;
    // A replay can take half an hour; a failed publish still prints its scores so the result is not lost.
    const scores = await publish(lf, runName, r, blocks, extra).catch((err: Error) => {
      throw new Error(`publish failed: ${err.message}; scores ${JSON.stringify({ ...scoresOf(r, blocks), ...extra })}`);
    });
    console.log(`${item.id} ${JSON.stringify(scores)}${r.error ? ` error: ${r.error}` : ''}`);
    return scores;
  });
  for (const f of failures) console.error(`${f.item.id} failed: ${f.error.message}`);
  console.log(`${runName}: ${summarize(rows)}${failures.length ? `; ${failures.length} of ${items.length} items failed` : ''}`);
  if (failures.length) process.exitCode = 1;
} else {
  throw new Error('usage: main.ts dataset <dataset> <agent> <good|bad|-> <session-dir>... | run [--concurrency=N] <dataset> <run-name> <deployment.json> <agent.json> [item-id...]');
}

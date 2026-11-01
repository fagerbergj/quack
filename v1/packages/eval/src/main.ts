// Usage: main.ts dataset <dataset> <agent> <good|bad|-> <session-dir>...
//        main.ts run [--concurrency=N] <dataset> <run-name> <deployment.json> <agent.json> [item-id...]
import { resolve } from 'node:path';
import { evalService, parseReview } from '@quack/github';
import { itemFrom, type Label } from './dataset.ts';
import { langfuse } from './langfuse.ts';
import { readSession } from './record.ts';
import { replay, type EvalServices } from './replay.ts';
import { publish, summarize, type Blocks } from './report.ts';

const services: EvalServices = { 'github.pr': (pin) => evalService(pin as never) };
const blocks: Blocks = (finals) => (finals['github.review'] === undefined ? undefined : parseReview(finals['github.review']).verdict === 'request_changes');
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
  // EV2: a benchmark that can promote an agent needs 20 real cases; a partial run still reports, it just cannot promote.
  if (!only.length && items.length < MIN_CASES) console.log(`note: ${dataset} has ${items.length} items; a promotion needs ${MIN_CASES} (EV2)`);
  const queue = [...items];
  const rows: Record<string, number>[] = [];
  await Promise.all(Array.from({ length: concurrency }, async () => {
    for (let item = queue.shift(); item; item = queue.shift()) {
      const r = await replay(item, { deployment: resolve(deployment), agent: resolve(agent) }, services);
      const scores = await publish(lf, runName, r, blocks);
      rows.push(scores);
      console.log(`${item.id} ${JSON.stringify(scores)}${r.error ? ` error: ${r.error}` : ''}`);
    }
  }));
  console.log(`${runName}: ${summarize(rows)}`);
} else {
  throw new Error('usage: main.ts dataset <dataset> <agent> <good|bad|-> <session-dir>... | run [--concurrency=N] <dataset> <run-name> <deployment.json> <agent.json> [item-id...]');
}

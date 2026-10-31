// Usage: main.ts dataset <dataset> <agent> <good|bad|-> <session-dir>...
//        main.ts run <dataset> <run-name> <deployment.json> <agent.json> [item-id...]
import { resolve } from 'node:path';
import { evalService } from '@quack/github';
import { itemFrom, type Label } from './dataset.ts';
import { langfuse } from './langfuse.ts';
import { readSession } from './record.ts';
import { replay, type EvalServices } from './replay.ts';
import { publish } from './report.ts';

const services: EvalServices = { 'github.pr': (pin) => evalService(pin as never) };
const MIN_CASES = 20;

function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const lf = langfuse({ baseUrl: env('LANGFUSE_BASE_URL'), publicKey: env('LANGFUSE_PUBLIC_KEY'), secretKey: env('LANGFUSE_SECRET_KEY') });
const [command, dataset, ...rest] = process.argv.slice(2);

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
  for (const item of items) {
    const r = await replay(item, { deployment: resolve(deployment), agent: resolve(agent) }, services);
    console.log(`${item.id} ${JSON.stringify(await publish(lf, runName, r))}${r.error ? ` error: ${r.error}` : ''}`);
  }
} else {
  throw new Error('usage: main.ts dataset <dataset> <agent> <good|bad|-> <session-dir>... | run <dataset> <run-name> <deployment.json> <agent.json> [item-id...]');
}

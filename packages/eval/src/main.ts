// Usage: main.ts dataset <dataset|items.json> <agent> <good|bad|-> <session-dir>...
//        main.ts run [--concurrency N] [--arm label=ref]... [--guides dir]... [--verdict v]... [--suggested] [--out file] <dataset|items.json> <run-name> <deployment.json> <agent.json> [item-id...]
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseArgs } from 'node:util';
import { itemFrom, type Label } from './dataset.ts';
import type { Policy, Verdict } from './driver.ts';
import { langfuse, type DatasetItem } from './langfuse.ts';
import type { ItemInput, ReplayResult } from './replay.ts';
import { eachArm } from './pool.ts';
import { publish, scoresOf, summarize } from './report.ts';

const MIN_CASES = 20;
const USAGE = 'usage: main.ts dataset <dataset|items.json> <agent> <good|bad|-> <session-dir>... | run [--concurrency N] [--arm label=ref]... [--guides dir]... [--verdict approve|stop|reject:<reason>]... [--suggested] [--out file] <dataset|items.json> <run-name> <deployment.json> <agent.json> [item-id...]';

function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const { values, positionals } = parseArgs({ allowPositionals: true, options: { concurrency: { type: 'string', default: '1' }, arm: { type: 'string', multiple: true }, guides: { type: 'string', multiple: true }, verdict: { type: 'string', multiple: true }, suggested: { type: 'boolean' }, out: { type: 'string' } } });
// vLLM serves several sequences at once; leave slots free for the deployment's live sessions.
const concurrency = Number(values.concurrency);
if (!Number.isInteger(concurrency) || concurrency < 1) throw new Error(`--concurrency must be a positive integer, got ${values.concurrency}`);
const [command, dataset, ...rest] = positionals;
// Checked before every extension loads, so a usage error exits at once.
if ((command !== 'dataset' && command !== 'run') || !dataset || rest.length < 3) throw new Error(USAGE);
const [{ loadManifests }, { adaptersOf, blocksOf, servicesOf }, { readSession }, { candidateAt, replay }] = await Promise.all([import('@quack/runtime/catalog'), import('./adapters.ts'), import('./record.ts'), import('./replay.ts')]);
const adapters = adaptersOf(await loadManifests());
const blocks = blocksOf(adapters);
const served = new Set(Object.keys(servicesOf(adapters)));
// A .json dataset is a local file of items, so a run needs no Langfuse and publishes nothing.
const local = dataset?.endsWith('.json');
const lf = () => langfuse({ baseUrl: env('LANGFUSE_BASE_URL'), publicKey: env('LANGFUSE_PUBLIC_KEY'), secretKey: env('LANGFUSE_SECRET_KEY') });
const readItems = (file: string): DatasetItem[] => (existsSync(file) ? JSON.parse(readFileSync(file, 'utf8')) : []);

function verdictOf(v: string): Verdict {
  if (v === 'approve' || v === 'stop') return v;
  if (v.startsWith('reject:')) return { reject: v.slice('reject:'.length) };
  throw new Error(`--verdict must be approve, stop or reject:<reason>, got ${v}`);
}

/** Each kind's versions as sealed, passed or failed with the gates that failed. */
const seals = (r: ReplayResult) => Object.entries(r.record.artifacts).map(([kind, versions]) => `${kind} ${versions.map((v) => {
  const verdict = v.verdict as { passed?: boolean; gatesFailed?: string[] } | null;
  return `v${v.version} ${!verdict ? 'unjudged' : verdict.passed ? 'pass' : `fail${verdict.gatesFailed?.length ? ` (${verdict.gatesFailed.join(', ')})` : ''}`}`;
}).join(', ')}`).join('; ');

async function addItems(agent: string, label: string, dirs: string[]) {
  const client = local ? undefined : lf();
  await client?.ensureDataset(dataset);
  const items = local ? readItems(dataset) : [];
  for (const dir of dirs) {
    const record = await readSession(dir);
    // A wrong path reads as an empty session; it must not become a benchmark case.
    if (!record.inputs.length) throw new Error(`${dir} has no recorded session`);
    // Only docs an eval service replays are pinned; the rest are named in metadata.unpinned and a run reads them live.
    const item = itemFrom(record, { agent, served, label: label === '-' ? undefined : (label as Label) });
    if (client) await client.upsertItem(dataset, item);
    else items.splice(0, items.length, ...items.filter((i) => i.id !== item.id), item);
    const live = (item.metadata as { unpinned: string[] }).unpinned;
    console.log(`${item.id} ${label} ${dir}${live.length ? ` (replays read the live world for ${live.join(', ')})` : ''}`);
  }
  if (local) writeFileSync(dataset, JSON.stringify(items, null, 2));
}

async function run(runName: string, deployment: string, agent: string, only: string[]) {
  const client = local ? undefined : lf();
  const items = (client ? await client.items(dataset) : readItems(dataset)).filter((i) => !only.length || only.includes(i.id));
  // A benchmark that can promote an agent needs 20 real cases; a partial run still reports, it just cannot promote.
  if (!only.length && items.length < MIN_CASES) console.log(`note: ${dataset} has ${items.length} items; a promotion needs ${MIN_CASES}`);
  const arms = (values.arm ?? ['current=.']).map((spec) => {
    const [label, ref = '.'] = spec.split('=');
    return { label, run: values.arm ? `${runName}-${label}` : runName, candidate: candidateAt(ref, resolve(deployment), resolve(agent), values.guides) };
  });
  const verdicts = values.verdict?.map(verdictOf);
  const policyOf = (item: DatasetItem): Policy => ({ ...(item.input as ItemInput).asks, ...(verdicts && { verdicts }), ...(values.suggested && { answers: [] }) });
  const { results, failures } = await eachArm(items, arms, concurrency, async ({ item, arm }) => {
    const r = await replay(item, arm.candidate, adapters, undefined, policyOf(item));
    const extra = r.codeScores;
    // A replay can take half an hour; a failed publish still prints its scores so the result is not lost.
    const scores = client ? await publish(client, arm.run, r, blocks, extra).catch((err: Error) => {
      throw new Error(`publish failed: ${err.message}; scores ${JSON.stringify({ ...scoresOf(r, blocks), ...extra })}`);
    }) : { ...scoresOf(r, blocks), ...extra };
    const asks = r.asks.map((a) => `${a.ask} ${a.about}: ${a.answer}`).join('; ');
    console.log(`${item.id} ${arm.label} ${JSON.stringify(scores)}${seals(r) ? ` seals: ${seals(r)}` : ''}${asks ? ` asks: ${asks}` : ''}${r.error ? ` error: ${r.error}` : ''}`);
    if (values.out) appendFileSync(values.out, `${JSON.stringify({ item: item.id, arm: arm.label, scores, asks: r.asks, stopped: r.stopped, error: r.error ?? null, outcome: r.record.outcome, artifacts: r.record.artifacts })}\n`);
    return { arm: arm.label, scores };
  });
  for (const f of failures) console.error(`${f.item.item.id} ${f.item.arm.label} failed: ${f.error.message}`);
  for (const arm of arms) {
    const failed = failures.filter((f) => f.item.arm === arm).length;
    console.log(`${arm.run}: ${summarize(results.filter((r) => r.arm === arm.label).map((r) => r.scores))}${failed ? `; ${failed} of ${items.length} items failed` : ''}`);
  }
  if (failures.length) process.exitCode = 1;
}

if (command === 'dataset') await addItems(rest[0], rest[1], rest.slice(2));
else await run(rest[0], rest[1], rest[2], rest.slice(3));

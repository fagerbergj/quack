import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels } from '@earendil-works/pi-ai';
import { createRegistry, Harness } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { collectFacts } from './facts.ts';
import type { IndexStore } from './store.ts';

const ctx = BACKGROUND_CONTEXT;
const text = (path: string) => readFile(path, 'utf8').catch(() => undefined);

// Replays every session log under dataDir into the index (A6); a log has one owner, so stop the distributor first.
export async function rebuild(store: IndexStore, dataDir: string, now = Date.now): Promise<number> {
  let sessions = 0;
  for (const d of await readdir(dataDir)) {
    const dir = join(dataDir, d);
    const key = await text(join(dir, 'session-key'));
    if (key === undefined) continue;
    const version = JSON.parse((await text(join(dir, 'agent-def', 'agent.json'))) ?? 'null')?.version;
    const meta = { key, agent: (await text(join(dir, 'agent'))) ?? '', version: typeof version === 'number' ? version : null };
    const harness = await Harness.open(await openNodeSqliteStorage(join(dir, 'session.sqlite')), { models: createModels(), registry: createRegistry() }, ctx);
    try {
      await store.upsert(key, await collectFacts(harness, await harness.root(ctx), meta, now(), ctx));
    } finally {
      await harness.close(ctx);
    }
    sessions++;
  }
  return sessions;
}

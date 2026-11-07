// The catalog's recovery: every session's recorded lifecycle stream, replayed with the action journal.
import { existsSync } from 'node:fs';
import { readdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels } from '@earendil-works/pi-ai';
import { createRegistry, Harness } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { parseEvents, type Catalog } from '@quack/catalog';
import { append, LifecycleStream, readStream, sessionFacts, type PriceOf, type WireEvent } from '@quack/session';

const ctx = BACKGROUND_CONTEXT;
const BACKFILLED = 'catalog-backfilled';

/** A session's recorded stream; one from before the stream is derived from its documents once and recorded in its log,
 * so the session continues the same numbering. A log has one owner: call this only while the session has no process. */
export async function recordedStream(dir: string, priceOf: PriceOf): Promise<WireEvent[] | undefined> {
  if (!existsSync(join(dir, 'session.sqlite'))) return undefined;
  const harness = await Harness.open(await openNodeSqliteStorage(join(dir, 'session.sqlite')), { models: createModels(), registry: createRegistry() }, ctx);
  try {
    const root = await harness.root(ctx);
    const { doc, recorded, fresh, legacyDirect } = await readStream(harness, root, await sessionFacts(dir), priceOf, ctx);
    if (recorded) return doc.events;
    await root.commit(async (tx) => {
      const d = await tx.doc(LifecycleStream, root.id);
      if (legacyDirect) d.direct = legacyDirect;
      append(d, fresh);
    }, ctx);
    return fresh;
  } finally {
    await harness.close(ctx);
  }
}

/** Rebuilds every session in the data dir from its log and the journal; a session that cannot be read is logged and skipped. */
export async function rebuildCatalog(catalog: Catalog, dataDir: string, priceOf: PriceOf, log = (m: string) => console.error(m)): Promise<number> {
  let rebuilt = 0;
  for (const d of await readdir(dataDir).catch(() => [] as string[])) {
    const dir = join(dataDir, d);
    const key = await readFile(join(dir, 'session-key'), 'utf8').catch(() => undefined);
    if (key === undefined) continue;
    try {
      const events = await recordedStream(dir, priceOf);
      if (!events) continue;
      await catalog.rebuild(key, parseEvents(events));
      // Called only while no session process runs, so every rebuilt session is stopped.
      await catalog.stopped(key);
      rebuilt++;
    } catch (err) {
      log(`catalog: session ${key} not rebuilt: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  return rebuilt;
}

/** On a deployment's first boot with the catalog, its existing sessions are rebuilt into it once; later boots skip this. */
export async function backfillCatalog(catalog: Catalog, dataDir: string, priceOf: PriceOf, log = (m: string) => console.error(m)): Promise<number | undefined> {
  if (existsSync(join(dataDir, BACKFILLED))) return undefined;
  // A store that is down fails the boot, and compose restarts it, rather than marking an empty backfill done.
  await catalog.ensure();
  const count = await rebuildCatalog(catalog, dataDir, priceOf, log);
  await writeFile(join(dataDir, BACKFILLED), new Date().toISOString());
  return count;
}

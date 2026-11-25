// A session's export, built alike by its own process and, for a stopped session, by the distributor from its store.
import { existsSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels } from '@earendil-works/pi-ai';
import { createRegistry, Harness, type Conversation } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { Artifacts, type SealedArtifact } from '@quack/sdk';

const ctx = BACKGROUND_CONTEXT;
const EXPORTED = new Set(['pi.user', 'pi.assistant', 'pi.tool-result']);

export type SessionExport = { transcript: Array<{ kind: string; model: unknown }>; artifacts: SealedArtifact[] };

/** The messages the transcript view shows, tool calls and results included; the system prompt and other log entries stay out. */
export async function transcriptOf(root: Conversation): Promise<SessionExport['transcript']> {
  const found: SessionExport['transcript'] = [];
  let cursor: unknown;
  do {
    const page = await root.commit((tx) => tx.scanEntries({ conversationId: root.id, order: 'ascending' }, 200, cursor as never), ctx);
    for (const e of page.items) if (EXPORTED.has(e.kind) && e.model?.length) found.push({ kind: e.kind, model: e.model });
    cursor = page.next as never;
  } while (cursor);
  return found;
}

/** Every sealed version of every kind, with its checker's verdict, oldest first within a kind. */
export async function sealedOf(harness: Harness, root: Conversation): Promise<SealedArtifact[]> {
  const doc = await harness.snapshot(Artifacts, root.id, ctx);
  return Object.values(doc?.kinds ?? {}).flatMap((k) => k.versions);
}

export const sessionExport = async (harness: Harness, root: Conversation): Promise<SessionExport> => ({ transcript: await transcriptOf(root), artifacts: await sealedOf(harness, root) });

/** The export of the session stored in `dir`, opening nothing for writing; throws when the store cannot be read. */
export const readExport = async (dir: string): Promise<SessionExport> => (await readStore(dir, sessionExport)) ?? { transcript: [], artifacts: [] };

/** Reads the session stored in `dir` without opening it for writing; undefined for a session that never ran, throws when the store cannot be read.
 * The store is copied in one read transaction first, since pi-durable's open writes; a process that starts meanwhile writes only the original. */
export async function readStore<T>(dir: string, read: (harness: Harness, root: Conversation) => Promise<T>): Promise<T | undefined> {
  const file = join(dir, 'session.sqlite');
  if (!existsSync(file)) return undefined;
  const tmp = await mkdtemp(join(tmpdir(), 'quack-export-'));
  try {
    const source = new DatabaseSync(file, { readOnly: true, timeout: 5_000 });
    try {
      source.prepare('VACUUM INTO ?').run(join(tmp, 'session.sqlite'));
    } finally {
      source.close();
    }
    const harness = await Harness.open(await openNodeSqliteStorage(join(tmp, 'session.sqlite')), { models: createModels(), registry: createRegistry(), readOnly: true }, ctx);
    try {
      return await read(harness, await harness.root(ctx));
    } finally {
      await harness.close(ctx);
    }
  } finally {
    await rm(tmp, { recursive: true, force: true });
  }
}

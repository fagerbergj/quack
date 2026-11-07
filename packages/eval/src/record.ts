import { existsSync } from 'node:fs';
import { readFile, realpath } from 'node:fs/promises';
import { join, relative, isAbsolute } from 'node:path';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import type { ConversationId, JsonObject } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import type { SealedArtifact } from '@quack/sdk';
import { runResults } from '@quack/workflows';

type Usage = { input: number; output: number; cacheRead: number };

/** What one recorded session did, read from its log without resuming it. */
export type SessionRecord = {
  inputs: string[];
  /** The last submission's outcome: its answer, or why it went unanswered. */
  outcome: { status: 'done'; answer: string } | { status: 'unanswered'; reason: string } | { status: 'pending' };
  turns: number;
  usage: Usage;
  artifacts: Record<string, SealedArtifact[]>;
  docs: Record<string, JsonObject>;
  results?: JsonObject;
  readFile?: (path: string) => Promise<string | undefined>;
};

const ROOT = 1 as unknown as ConversationId;

function text(content: unknown): string {
  if (typeof content === 'string') return content;
  return Array.isArray(content) ? content.map((c) => (c?.type === 'text' ? c.text : '')).join('') : '';
}

async function* pages<T>(scan: (cursor: unknown) => Promise<{ items: readonly T[]; next?: unknown }>) {
  let cursor: unknown;
  do {
    const page = await scan(cursor);
    yield* page.items;
    cursor = page.next;
  } while (cursor);
}

type Storage = Awaited<ReturnType<typeof openNodeSqliteStorage>>;
type Submission = { status: string; answer?: unknown; reason?: string };

async function readTranscript(storage: Storage, record: SessionRecord, last: Submission | undefined) {
  for await (const e of pages((c) => storage.scanEntries({ conversationId: ROOT, order: 'ascending' }, 200, c as never, ctx))) {
    const m = e.model?.[0] as { role?: string; content?: unknown; usage?: Usage } | undefined;
    if (e.kind === 'pi.user') record.inputs.push(text(m?.content));
    if (e.kind !== 'pi.assistant') continue;
    if (last?.status === 'done' && e.id === last.answer) record.outcome = { status: 'done', answer: text(m?.content) };
    record.turns++;
    for (const k of ['input', 'output', 'cacheRead'] as const) record.usage[k] += m?.usage?.[k] ?? 0;
  }
}

async function readDocs(storage: Storage, record: SessionRecord) {
  for await (const d of pages((c) => storage.scanDocuments({ scope: { kind: 'conversation', conversationId: ROOT }, at: 'current' }, 50, c as never, ctx))) {
    const value = (await storage.document(d.id, 'current', ctx))?.value;
    if (!value) continue;
    if (d.kind === 'quack.artifacts') {
      for (const [kind, { versions }] of Object.entries((value as { kinds: Record<string, { versions: SealedArtifact[] }> }).kinds)) record.artifacts[kind] = versions;
    } else record.docs[d.kind] = value;
  }
}

async function readResults(storage: Storage, record: SessionRecord) {
  const doc = record.docs['quack.workflow'] as { task?: string | null; kept?: JsonObject | null } | undefined;
  const task = doc?.task ? await storage.task(Number(doc.task) as never, ctx) : undefined;
  if (task) record.results = runResults(doc!, task.state as never) as JsonObject | undefined;
}

/** Reads a workspace file lazily, so the directory must outlive the record; a path that resolves outside the workspace reads as absent. */
const workspaceReader = (dir: string) => async (path: string): Promise<string | undefined> => {
  const root = join(dir, 'workspace');
  try {
    const real = await realpath(join(root, path));
    const rel = relative(await realpath(root), real);
    return rel.startsWith('..') || isAbsolute(rel) ? undefined : await readFile(real, 'utf8');
  } catch {
    return undefined;
  }
};

const empty = (): SessionRecord => ({ inputs: [], outcome: { status: 'pending' }, turns: 0, usage: { input: 0, output: 0, cacheRead: 0 }, artifacts: {}, docs: {} });

export async function readSession(dir: string): Promise<SessionRecord> {
  // A session whose runtime never started has no log; opening one would create an empty database instead.
  const file = join(dir, 'session.sqlite');
  if (!existsSync(file)) return empty();
  const storage = await openNodeSqliteStorage(file);
  try {
    let last: Submission | undefined;
    for await (const s of pages((c) => storage.scanSubmissions({ conversationId: ROOT }, 50, c as never, ctx))) if (s.type === 'input') last = s;
    const record = empty();
    await readTranscript(storage, record, last);
    if (last?.status === 'unanswered') record.outcome = { status: 'unanswered', reason: last.reason ?? '' };
    await readDocs(storage, record);
    await readResults(storage, record);
    record.readFile = workspaceReader(dir);
    return record;
  } finally {
    await storage.close(ctx);
  }
}

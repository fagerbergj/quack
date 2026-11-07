import { existsSync } from 'node:fs';
import { readFile, realpath } from 'node:fs/promises';
import { join, relative, isAbsolute } from 'node:path';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import type { ConversationId, JsonObject } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import type { SealedArtifact } from '@quack/sdk';
import { isWorkflowRun, runResults } from '@quack/workflows';

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
  /** Every workflow run, oldest first, with the step results its steps saw. */
  runs: Run[];
  results?: JsonObject;
  readFile?: (path: string) => Promise<string | undefined>;
};

type Run = { startedAt?: number; results?: JsonObject };

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

// Only the latest run's failure is kept in the workflow doc, so an earlier failed run has no results.
async function readRuns(storage: Storage, record: SessionRecord) {
  const doc = record.docs['quack.workflow'] as { task?: string | null; kept?: JsonObject | null } | undefined;
  for await (const t of pages((c) => storage.scanTasks({ conversationId: ROOT }, 50, c as never, ctx))) {
    if (!isWorkflowRun(t.kind)) continue;
    const results = runResults(doc?.task === String(t.id) ? doc : {}, t.state as never) as JsonObject | undefined;
    record.runs.push({ startedAt: t.startedAt, results });
  }
}

/** The run that sealed a version: the newest started by its seal time, or the only run when start times were not recorded. */
function runAt(runs: Run[], sealedAt: number): Run | undefined {
  if (runs.length === 1 && runs[0].startedAt === undefined) return runs[0];
  return runs.filter((r) => r.startedAt !== undefined && r.startedAt <= sealedAt).at(-1);
}

/** The session as its judge saw one version of a kind (the last when none is named): later versions cut, and the results of the run that sealed it. */
export function asJudged(record: SessionRecord, kind: string, version?: number): SessionRecord {
  const versions = record.artifacts[kind] ?? [];
  const sealed = version ? versions.find((v) => v.version === version) : versions.at(-1);
  if (!sealed) return record;
  return { ...record, artifacts: { ...record.artifacts, [kind]: versions.filter((v) => v.version <= sealed.version) }, results: runAt(record.runs, sealed.sealedAt)?.results };
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

const empty = (): SessionRecord => ({ inputs: [], outcome: { status: 'pending' }, turns: 0, usage: { input: 0, output: 0, cacheRead: 0 }, artifacts: {}, docs: {}, runs: [] });

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
    await readRuns(storage, record);
    record.readFile = workspaceReader(dir);
    return record;
  } finally {
    await storage.close(ctx);
  }
}

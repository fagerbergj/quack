import { join } from 'node:path';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import type { ConversationId, JsonObject } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import type { SealedArtifact } from '@quack/sdk';

type Usage = { input: number; output: number; cacheRead: number };

/** What one recorded session did, read from its log without resuming it (OBS-4). */
export type SessionRecord = {
  inputs: string[];
  /** The last submission's outcome: its answer, or why it went unanswered. */
  outcome: { status: 'done'; answer: string } | { status: 'unanswered'; reason: string } | { status: 'pending' };
  turns: number;
  usage: Usage;
  artifacts: Record<string, SealedArtifact[]>;
  docs: Record<string, JsonObject>;
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

export async function readSession(dir: string): Promise<SessionRecord> {
  const storage = await openNodeSqliteStorage(join(dir, 'session.sqlite'));
  try {
    let last: { status: string; answer?: unknown; reason?: string } | undefined;
    for await (const s of pages((c) => storage.scanSubmissions({ conversationId: ROOT }, 50, c as never, ctx))) if (s.type === 'input') last = s;
    const record: SessionRecord = { inputs: [], outcome: { status: 'pending' }, turns: 0, usage: { input: 0, output: 0, cacheRead: 0 }, artifacts: {}, docs: {} };
    for await (const e of pages((c) => storage.scanEntries({ conversationId: ROOT, order: 'ascending' }, 200, c as never, ctx))) {
      const m = e.model?.[0] as { role?: string; content?: unknown; usage?: Usage } | undefined;
      if (e.kind === 'pi.user') record.inputs.push(text(m?.content));
      if (e.kind !== 'pi.assistant') continue;
      if (last?.status === 'done' && e.id === last.answer) record.outcome = { status: 'done', answer: text(m?.content) };
      record.turns++;
      for (const k of ['input', 'output', 'cacheRead'] as const) record.usage[k] += m?.usage?.[k] ?? 0;
    }
    if (last?.status === 'unanswered') record.outcome = { status: 'unanswered', reason: last.reason ?? '' };
    for await (const d of pages((c) => storage.scanDocuments({ scope: { kind: 'conversation', conversationId: ROOT }, at: 'current' }, 50, c as never, ctx))) {
      const value = (await storage.document(d.id, 'current', ctx))?.value;
      if (!value) continue;
      if (d.kind === 'quack.artifacts') {
        for (const [kind, { versions }] of Object.entries((value as { kinds: Record<string, { versions: SealedArtifact[] }> }).kinds)) record.artifacts[kind] = versions;
      } else record.docs[d.kind] = value;
    }
    return record;
  } finally {
    await storage.close(ctx);
  }
}

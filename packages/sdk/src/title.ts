import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';

// The lifecycle channel bounds title.set's title at its source.
const TITLE_MAX = 120;

/** A session's default title (null until set), how many times it changed (names title.set `title:<changes>`) and when.
 * Core's lifecycle reads it to derive the stream. */
export type SessionTitleDoc = { title: string | null; changes: number; at: number };

export const SessionTitle = defineDoc<SessionTitleDoc>({
  kind: 'quack.session-title', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ title: null, changes: 0, at: 0 }), checkpointWhen: () => false,
});

/** Sets the calling extension's own session's default title, last write wins; each change goes out as title.set `title:<n>`
 * and a higher seq supersedes, same id or not. The same title again (a replayed step) writes nothing. */
export async function setDefaultSessionTitle(api: ToolExecutionApi, title: string, ctx: Context): Promise<void> {
  const folded = title.trim().replace(/\s+/g, ' ').slice(0, TITLE_MAX);
  if (!folded) return;
  await api.commit(async (tx) => {
    const doc = await tx.doc(SessionTitle, api.conversationId);
    if (doc.title === folded) return;
    doc.title = folded;
    doc.changes += 1;
    doc.at = api.now();
  }, ctx);
}

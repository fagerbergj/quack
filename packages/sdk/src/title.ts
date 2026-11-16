import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';

// The lifecycle channel bounds title.set's title at its source.
const TITLE_MAX = 120;

/** The default title an extension set for its own session; `changes` counts the times it changed, and names its title.set.
 * Not on the SDK's public surface: core reads the document by its kind (packages/session/src/lifecycle.ts). */
export type SessionTitleDoc = { title: string | null; changes: number; at: number };

export const SessionTitle = defineDoc<SessionTitleDoc>({
  kind: 'quack.session-title', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ title: null, changes: 0, at: 0 }), checkpointWhen: () => false,
});

/** Sets the calling extension's own session's default title, last write wins; the same title again (a replayed step) writes nothing. */
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

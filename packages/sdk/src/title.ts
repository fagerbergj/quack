import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';

// The lifecycle channel bounds title.set's title at its source.
const TITLE_MAX = 120;

/**
 * The default title an extension set for its own session: the title, or null until one is
 * set; `changes` counts the times it changed and names its title.set (`title:<changes>`);
 * `at` the moment of the last change. One record per conversation, starting at
 * { title: null, changes: 0, at: 0 }.
 * Provided by the sdk (kind 'quack.session-title'); consumed by setDefaultSessionTitle,
 * which writes it, and by core's lifecycle, which reads it when it derives the stream.
 */
export type SessionTitleDoc = { title: string | null; changes: number; at: number };

/**
 * The conversation-scoped document of a session's default title: kind 'quack.session-title',
 * latest history, initial { title: null, changes: 0, at: 0 }.
 * Provided by the sdk; consumed by setDefaultSessionTitle and by core's lifecycle.
 */
export const SessionTitle = defineDoc<SessionTitleDoc>({
  kind: 'quack.session-title', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ title: null, changes: 0, at: 0 }), checkpointWhen: () => false,
});

/**
 * Sets the default title of the calling extension's own session: last write wins. Core
 * persists it in the session's title document, prefers it over the derived title (the
 * write-once title file, or the first input), and re-emits title.set, each change under its
 * own id `title:<n>`, n the number of changes: the catalog applies an event above the
 * session's seq watermark, so a higher seq supersedes, same id or not, and a recovered
 * session re-sends only the newest. Whitespace folded, trimmed and cut to 120, like a
 * derived title; a person's custom title is never touched; ignored for home: sessions; a
 * replay of the calling step with the same title writes nothing, so the call is idempotent.
 * Provided by the sdk; consumed by an extension's session half, for its own session only:
 * the api is the caller's own conversation, and that is what limits the call to that session;
 * the contracts extension sets it at a spec's first successful seal and when a later
 * version's title changes. It is not a model tool and it is not on HTTP.
 */
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

import { createHash } from 'node:crypto';
import { obj, str } from '@earendil-works/pi-schema';
import { defineTool } from '@earendil-works/pi-durable';
import { Artifacts, type SealedArtifact } from '@quack/sdk';

/** The agent's reply to one review comment, sealed in its own session: `{ comment, text }`. */
export const REPLY_KIND = 'contract.reply';
export const MAX_REPLIES = 20;

const reply = (text: string, isError = false) => ({ output: [{ type: 'text' as const, text }], isError });
const commentOf = (v: SealedArtifact) => { try { return (JSON.parse(v.content) as { comment?: unknown }).comment; } catch { return undefined; } };

/** reply_comment: seals a contract.reply the person sees beside their comment; a 21st reply to one comment is refused. */
export const replyCommentTool = defineTool({
  name: 'reply_comment',
  description: 'Answer one review comment from the person\'s Ask for changes, by its id in the ```quack-review block: say what you changed for it, or why you did not change anything. The person sees the reply beside their comment. A comment you address by changing the spec is named in the addresses of the changes row that changes it instead, or as well.',
  parameters: obj({
    id: str({ minLength: 1, maxLength: 200, description: 'The comment\'s id, as the quack-review block gives it' }),
    text: str({ minLength: 1, maxLength: 4000, description: 'Your reply, 1 to 4000 characters' }),
  }, { open: true }),
  async execute({ id, text }, api, ctx) {
    let refused = false;
    await api.commit(async (tx) => {
      const doc = await tx.doc(Artifacts, api.conversationId);
      // Read back through the draft: the object `??=` yields is not the draft, so a push to it is lost.
      doc.kinds[REPLY_KIND] ??= { versions: [] };
      const entry = doc.kinds[REPLY_KIND];
      refused = entry.versions.filter((v) => commentOf(v) === id).length >= MAX_REPLIES;
      if (refused) return;
      const content = JSON.stringify({ comment: id, text });
      entry.versions.push({ kind: REPLY_KIND, version: entry.versions.length + 1, hash: createHash('sha256').update(content).digest('hex'), content, sealedAt: api.now(), verdict: null, summary: text.slice(0, 80) });
    }, ctx);
    return refused ? reply(`Comment ${id} already has ${MAX_REPLIES} replies, the most it takes; say anything more in the spec.`, true) : reply(`Replied to comment ${id}.`);
  },
});

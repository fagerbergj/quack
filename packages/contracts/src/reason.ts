// The review reason of the Contracts app API v13: the note when given, a blank line, then one ```quack-review block. No node: imports.

/** Where a comment is made: a document pointer, a choice, the changes rows of a choice, or an intent entry. */
export type Anchor = { on: 'document'; ptr: string } | { on: 'choice'; id: string } | { on: 'change'; choice: string } | { on: 'guarantee' | 'nonGoal'; text: string; id?: string };
/** The block's single line: the review, the artifact it rejects, and each comment's id, anchor and body. */
export type ReviewBlock = { review: string; artifact: number; comments: Array<{ id: string; anchor: Anchor; body: string }> };

export const OPEN_FENCE = '```quack-review';
// JSON escapes newlines, so the block's line can hold no fence; the opening and closing fences stand on lines of their own.
const BLOCK = /(?:^|\n)```quack-review\n([^\n]*)\n```(?=\n|$)/g;

/** The exact reason a review sends: the note, a blank line, then the block, which ends it. */
export const reviewReason = (note: string | undefined, block: ReviewBlock) => `${note ? `${note}\n\n` : ''}${OPEN_FENCE}\n${JSON.stringify(block)}\n\`\`\``;

/** The last quack-review block in a reason, with the text before it; undefined when none parses. */
export function lastReviewBlock(reason: string | null | undefined): { note: string; block: ReviewBlock } | undefined {
  const last = [...(reason ?? '').matchAll(BLOCK)].at(-1);
  if (!last) return undefined;
  let block: unknown;
  try { block = JSON.parse(last[1]!); } catch { return undefined; }
  const comments = (block as ReviewBlock | null)?.comments;
  if (!Array.isArray(comments) || !comments.every((c) => typeof c?.id === 'string')) return undefined;
  return { note: reason!.slice(0, last.index).trim(), block: block as ReviewBlock };
}

/** A reason as one line for a list: a review's note and how many comments it sent, else the reason itself. */
export function reasonSummary(reason: string): string {
  const r = lastReviewBlock(reason);
  if (!r) return reason;
  const n = r.block.comments.length;
  return `${r.note ? `${r.note.replace(/\s+/g, ' ')} · ` : ''}${n} review comment${n === 1 ? '' : 's'}`;
}

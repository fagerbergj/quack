import { createHmac, timingSafeEqual } from 'node:crypto';

/** Verifies GitHub's X-Hub-Signature-256 over the raw body in constant time. */
export function verifySignature(secret: string, body: Buffer, header: string | undefined): boolean {
  if (!header?.startsWith('sha256=')) return false;
  const expected = Buffer.from(`sha256=${createHmac('sha256', secret).update(body).digest('hex')}`);
  const given = Buffer.from(header);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

/** A run to start: which session, in which role, with what input. */
export type Trigger = { sessionKey: string; role: 'reviewer'; input: string; repo: { owner: string; name: string }; number: number; title: string; /** A mention: delivered behind a running review instead of starting one. */ followUp?: boolean };

const TRUSTED = new Set(['OWNER', 'MEMBER', 'COLLABORATOR']);

type Payload = Record<string, any>;

function reviewTrigger(p: Payload, pr: Payload, why: string): Trigger {
  const owner = p.repository.owner.login;
  const name = p.repository.name;
  const input = [
    `Review pull request ${owner}/${name}#${pr.number} (${why}).`,
    `Title: ${pr.title ?? ''}`,
    pr.head?.sha ? `Head: ${pr.head.sha} on ${pr.head.ref}; base: ${pr.base?.ref}` : '',
    '',
    pr.body ?? '',
  ].filter((l, i) => i !== 2 || l).join('\n');
  return { sessionKey: `github:${owner}/${name}#${pr.number}`, role: 'reviewer', input, repo: { owner, name }, number: pr.number, title: String(pr.title ?? '') };
}

// @user or @org/team; the leading class skips emails and URL paths. Scoped packages (@angular/core) match too, as GitHub pings them alike.
const mentionOf = (slug: string) => new RegExp(`(^|[^\\w/.:@])@${slug.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?![\\w-])`, 'gi');

/** Drops the "@" from the bot's own handle outside code, so nothing quack posts can re-trigger it. An unterminated fence or span is prose, which fails safe. */
export function stripMentions(text: string, slug: string): string {
  const re = mentionOf(slug);
  return text.split(/(```[\s\S]*?```|`[^`\n]*`)/).map((part, i) => (i % 2 ? part : part.replace(re, `$1${slug}`))).join('');
}

function mentionTrigger(event: string | undefined, p: Payload, slug: string): Trigger | undefined {
  const onReview = event === 'pull_request_review_comment';
  if (p.action !== 'created' || !(onReview || (event === 'issue_comment' && p.issue?.pull_request))) return undefined;
  const c = p.comment;
  const mentioned = mentionOf(slug).test(c?.body ?? '');
  // A bot author is quack itself (or another app): never a reason to run.
  if (!mentioned || !TRUSTED.has(c.author_association) || c.user?.type === 'Bot' || c.user?.login?.toLowerCase() === `${slug.toLowerCase()}[bot]`) return undefined;
  const pr = onReview ? p.pull_request : p.issue;
  const owner = p.repository.owner.login;
  const name = p.repository.name;
  const thread = onReview ? (c.in_reply_to_id ?? c.id) : c.id;
  const reply = `reply_mention with repository ${owner}/${name}, number ${pr.number}, kind ${onReview ? 'review' : 'issue'}, commentId ${thread}`;
  const input = [`${c.user?.login} mentioned you in ${onReview ? 'a review comment thread' : 'a comment'} on ${owner}/${name}#${pr.number}: ${c.html_url}`, `Answer with ${reply}.`, '', c.body].join('\n');
  return { sessionKey: `github:${owner}/${name}#${pr.number}`, role: 'reviewer', input, repo: { owner, name }, number: pr.number, title: '', followUp: true };
}

// G1/G5: the review label starts the first review; `/review` from a trusted commenter starts later ones; a trusted mention is a question for the running session.
export function triggerFor(event: string | undefined, p: Payload, label: string, slug: string): Trigger | undefined {
  if (event === 'pull_request' && p.action === 'labeled' && p.label?.name === label) return reviewTrigger(p, p.pull_request, `label ${label}`);
  const isReviewComment = event === 'issue_comment' && p.action === 'created' && p.issue?.pull_request && p.comment?.body?.trim() === '/review';
  if (isReviewComment && TRUSTED.has(p.comment.author_association)) return reviewTrigger(p, p.issue, `/review by ${p.comment.user?.login}`);
  return mentionTrigger(event, p, slug);
}

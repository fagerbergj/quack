import { createHmac } from 'node:crypto';
import { safeEqual } from '@quack/sdk';

/** Verifies GitHub's X-Hub-Signature-256 over the raw body in constant time. */
export function verifySignature(secret: string, body: Buffer, header: string | undefined): boolean {
  if (!header?.startsWith('sha256=')) return false;
  return safeEqual(header, `sha256=${createHmac('sha256', secret).update(body).digest('hex')}`);
}

/** A run to start: which session, in which role, with what input. */
export type Trigger = { sessionKey: string; role: 'reviewer' | 'implementer'; input: string; repo: { owner: string; name: string }; number: number; title: string; /** A mention: delivered behind a running review instead of starting one. */ followUp?: boolean; /** The run starts from an issue, so a refusal is worded for one. */ issue?: boolean };

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

// The PR session's transcript may hold the reviewer's run; this keeps the implementer on its own tools and the review feedback.
const CHECKOUT_FIRST = 'Start with checkout_task: it checks out the branch and returns the pull request\'s open review feedback. Use only your own tools; ignore tool calls in earlier turns by another agent.';

const FIX = /^\/fix(?:\s([\s\S]*))?$/;

// A trusted `/fix` on a pull request; what follows it is the instruction.
function fixTrigger(event: string | undefined, p: Payload): Trigger | undefined {
  const c = p.comment;
  const m = event === 'issue_comment' && p.action === 'created' && p.issue?.pull_request ? FIX.exec(String(c?.body ?? '').trim()) : null;
  if (!m || !TRUSTED.has(c.author_association)) return undefined;
  const owner = p.repository.owner.login;
  const name = p.repository.name;
  const input = [`Fix pull request ${owner}/${name}#${p.issue.number} (/fix by ${c.user?.login}).`, `Title: ${p.issue.title ?? ''}`, '', CHECKOUT_FIRST, '', `Instruction: ${m[1]?.trim() || '(none; fix failing CI and open review findings)'}`].join('\n');
  return { sessionKey: `github:${owner}/${name}#${p.issue.number}`, role: 'implementer', input, repo: { owner, name }, number: p.issue.number, title: String(p.issue.title ?? '') };
}

const IMPLEMENT = /^\/implement(?:\s([\s\S]*))?$/;
const ISSUE_BODY_CHARS = 8000;

// A trusted `/implement` on an issue (not a pull request); what follows it is extra instruction.
function implementTrigger(event: string | undefined, p: Payload): Trigger | undefined {
  const c = p.comment;
  const m = event === 'issue_comment' && p.action === 'created' && p.issue && !p.issue.pull_request ? IMPLEMENT.exec(String(c?.body ?? '').trim()) : null;
  if (!m || !TRUSTED.has(c.author_association)) return undefined;
  const owner = p.repository.owner.login;
  const name = p.repository.name;
  const i = p.issue;
  const input = [
    `Implement issue ${owner}/${name}#${i.number} (/implement by ${c.user?.login}); open a pull request that closes it.`,
    `Title: ${i.title ?? ''}`,
    `Labels: ${(i.labels ?? []).map((l: Payload) => l.name).join(', ') || 'none'}`,
    '', String(i.body ?? '').slice(0, ISSUE_BODY_CHARS), '', CHECKOUT_FIRST, '',
    `Instruction: ${m[1]?.trim() || '(none; implement the issue as written)'}`,
  ].join('\n');
  return { sessionKey: `github:${owner}/${name}#${i.number}`, role: 'implementer', input, repo: { owner, name }, number: i.number, title: String(i.title ?? ''), issue: true };
}

// G1/G5: the review label starts the first review, and a push to a labelled PR starts one for the new head; `/review` from a trusted commenter starts later ones; a trusted mention is a question for the running session.
export function triggerFor(event: string | undefined, p: Payload, label: string, slug: string): Trigger | undefined {
  if (event === 'pull_request' && p.action === 'labeled' && p.label?.name === label) return reviewTrigger(p, p.pull_request, `label ${label}`);
  // Without this a force-push after an approval left the new head unreviewed; checkout_pr ends a turn whose head was already posted.
  if (event === 'pull_request' && p.action === 'synchronize' && p.pull_request?.labels?.some((l: Payload) => l.name === label)) return reviewTrigger(p, p.pull_request, `new commits pushed to a ${label} PR`);
  const fix = fixTrigger(event, p) ?? implementTrigger(event, p);
  if (fix) return fix;
  const isReviewComment = event === 'issue_comment' && p.action === 'created' && p.issue?.pull_request && p.comment?.body?.trim() === '/review';
  if (isReviewComment && TRUSTED.has(p.comment.author_association)) return reviewTrigger(p, p.issue, `/review by ${p.comment.user?.login}`);
  return mentionTrigger(event, p, slug);
}

const CLOSES = /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)/gi;

/** The issue numbers a pull request body closes, by GitHub's closing keywords anywhere in the text. */
export const closedIssues = (body: string | undefined) => [...new Set([...String(body ?? '').matchAll(CLOSES)].map((m) => Number(m[1])))];

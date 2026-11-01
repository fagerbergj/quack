import { createHmac, timingSafeEqual } from 'node:crypto';

/** Verifies GitHub's X-Hub-Signature-256 over the raw body in constant time. */
export function verifySignature(secret: string, body: Buffer, header: string | undefined): boolean {
  if (!header?.startsWith('sha256=')) return false;
  const expected = Buffer.from(`sha256=${createHmac('sha256', secret).update(body).digest('hex')}`);
  const given = Buffer.from(header);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

/** A run to start: which session, in which role, with what input. */
export type Trigger = { sessionKey: string; role: 'reviewer'; input: string; repo: { owner: string; name: string }; number: number; title: string };

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

// G1/G5: the review label starts the first review; `/review` from a trusted commenter starts later ones.
export function triggerFor(event: string | undefined, p: Payload, label: string): Trigger | undefined {
  if (event === 'pull_request' && p.action === 'labeled' && p.label?.name === label) return reviewTrigger(p, p.pull_request, `label ${label}`);
  const isReviewComment = event === 'issue_comment' && p.action === 'created' && p.issue?.pull_request && p.comment?.body?.trim() === '/review';
  if (isReviewComment && TRUSTED.has(p.comment.author_association)) return reviewTrigger(p, p.issue, `/review by ${p.comment.user?.login}`);
  return undefined;
}

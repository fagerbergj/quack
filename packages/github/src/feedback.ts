import type { GithubApp } from './app.ts';

/** The review feedback still open on a pull request: what a person or another session asked for, not yet answered. */
export type ReviewFeedback = {
  reviews: Array<{ author: string; state: 'CHANGES_REQUESTED' | 'COMMENTED'; commit: string; /** The review was made on the current head. */ current: boolean; body: string }>;
  /** First comment of each unresolved review thread. */
  comments: Array<{ author: string; path: string; line: number | null; body: string }>;
  /** Why the threads could not be read; the comments above are then incomplete. */
  commentsError?: string;
  /** The app's own inline comments, newest first, each with its newest replies; absent from older records. */
  ownThreads?: OwnThread[];
  /** Own comments left out past the cap, and why the comments could not be read. */
  ownThreadsCut?: number;
  ownThreadsError?: string;
};

export type OwnThread = { id: number; path: string; line: number | null; commit: string; at: string; /** GitHub no longer shows it on the diff: the commented code changed. */ outdated: boolean; body: string; replies: Array<{ author: string; at: string; body: string }>; repliesCut: number };

export const MAX_OWN_THREADS = 20;
export const MAX_THREAD_REPLIES = 5;

const THREADS = `query($owner:String!,$repo:String!,$n:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$n){reviewThreads(first:100){nodes{isResolved path line originalLine comments(first:1){nodes{body author{login}}}}}}}}`;

/** Each reviewer's latest review that still asks for something; an approval clears the reviewer's earlier request. */
export function openReviews(reviews: any[], headSha: string): ReviewFeedback['reviews'] {
  const latest = new Map<string, any>();
  for (const r of [...reviews].sort((a, b) => String(a.submitted_at ?? '').localeCompare(String(b.submitted_at ?? '')))) {
    const counts = r.state === 'CHANGES_REQUESTED' || r.state === 'APPROVED' || (r.state === 'COMMENTED' && r.body?.trim());
    if (counts && r.user?.login) latest.set(r.user.login, r);
  }
  return [...latest.values()].filter((r) => r.state !== 'APPROVED')
    .map((r) => ({ author: r.user.login, state: r.state, commit: r.commit_id, current: r.commit_id === headSha, body: r.body ?? '' }));
}

// Resolution exists only in GraphQL. shortcut: the first 100 threads; a busier PR shows its oldest.
async function openComments(app: GithubApp, owner: string, repo: string, number: number): Promise<ReviewFeedback['comments']> {
  const { json } = await app.asInstallation(owner, repo, 'POST', '/graphql', { query: THREADS, variables: { owner, repo, n: number } });
  const nodes = json?.data?.repository?.pullRequest?.reviewThreads?.nodes;
  if (json?.errors?.length || !nodes) throw new Error(json?.errors?.[0]?.message ?? 'no review threads returned');
  return nodes.filter((t: any) => !t.isResolved && t.comments.nodes[0])
    .map((t: any) => ({ author: t.comments.nodes[0].author?.login ?? 'unknown', path: t.path, line: t.line ?? t.originalLine ?? null, body: t.comments.nodes[0].body }));
}

/** `login`'s top-level review comments, newest first, each with its newest replies; GitHub points every reply at the thread's first comment. */
export function ownThreads(comments: any[], login: string, maxThreads = MAX_OWN_THREADS, maxReplies = MAX_THREAD_REPLIES): { threads: OwnThread[]; cut: number } {
  const byAt = (a: any, b: any) => String(b.created_at).localeCompare(String(a.created_at));
  const mine = comments.filter((c) => !c.in_reply_to_id && String(c.user?.login).toLowerCase() === login.toLowerCase()).sort(byAt);
  const threads = mine.slice(0, maxThreads).map((c) => {
    const replies = comments.filter((r) => r.in_reply_to_id === c.id).sort(byAt);
    return {
      id: c.id, path: c.path, line: c.line ?? c.original_line ?? null, commit: String(c.original_commit_id ?? c.commit_id), at: c.created_at, outdated: c.line == null, body: c.body ?? '',
      replies: replies.slice(0, maxReplies).reverse().map((r) => ({ author: r.user?.login ?? 'unknown', at: r.created_at, body: r.body ?? '' })),
      repliesCut: Math.max(0, replies.length - maxReplies),
    };
  });
  return { threads, cut: mine.length - threads.length };
}

const MAX_REVIEW_PAGES = 30;
const MAX_COMMENT_PAGES = 10;

async function allPages(app: GithubApp, owner: string, repo: string, path: string, maxPages: number): Promise<any[]> {
  const all: any[] = [];
  for (let page = 1; page <= maxPages; page++) {
    const batch = (await app.asInstallation(owner, repo, 'GET', `${path}per_page=100&page=${page}`)).json ?? [];
    all.push(...batch);
    if (batch.length < 100) break;
  }
  return all;
}

// Every page, since the list is oldest first and the newest review is the one that counts. shortcut: 3,000 reviews unless maxPages says more.
export const allReviews = (app: GithubApp, owner: string, repo: string, base: string, maxPages = MAX_REVIEW_PAGES) => allPages(app, owner, repo, `${base}/reviews?`, maxPages);

// Newest first, so the page cap drops the oldest. shortcut: 1,000 comments; a reply whose thread start is older is not shown.
const reviewComments = (app: GithubApp, owner: string, repo: string, base: string) => allPages(app, owner, repo, `${base}/comments?sort=created&direction=desc&`, MAX_COMMENT_PAGES);

export async function reviewFeedback(app: GithubApp, owner: string, repo: string, number: number, headSha: string): Promise<ReviewFeedback> {
  const base = `/repos/${owner}/${repo}/pulls/${number}`;
  const found: ReviewFeedback = { reviews: openReviews(await allReviews(app, owner, repo, base), headSha), comments: [] };
  try {
    found.comments = await openComments(app, owner, repo, number);
  } catch (err) {
    found.commentsError = (err as Error).message.slice(0, 200);
  }
  try {
    const { threads, cut } = ownThreads(await reviewComments(app, owner, repo, base), `${await app.slug()}[bot]`);
    Object.assign(found, { ownThreads: threads, ownThreadsCut: cut });
  } catch (err) {
    found.ownThreadsError = (err as Error).message.slice(0, 200);
  }
  return found;
}

const BUDGET = 6_000;
const EACH = 1_500;
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)} [cut ${s.length - n} chars]` : s);

// Items in order until the budget runs out, each clipped; the rest are counted, not dropped silently.
function fit(items: string[], budget: number, each = EACH): string[] {
  const shown: string[] = [];
  let used = 0;
  for (const item of items) {
    const text = clip(item, each);
    if (used + text.length > budget) break;
    shown.push(text);
    used += text.length;
  }
  return [...shown, ...(shown.length < items.length ? [`(${items.length - shown.length} more review items cut to fit the size budget; read them on the pull request)`] : [])];
}

/** The feedback as text for the implementer, within a total character budget. */
export function renderFeedback(f: ReviewFeedback | undefined, budget = BUDGET): string {
  if (!f) return '';
  const lines = fit([
    ...f.reviews.map((r) => `- ${r.author} ${r.state} on ${r.commit.slice(0, 7)} (${r.current ? 'current head' : 'an older head'})${r.body.trim() ? `:\n${r.body.trim()}` : ''}`),
    ...f.comments.map((c) => `- ${c.author} on ${c.path}${c.line ? `:${c.line}` : ''}:\n${c.body.trim()}`),
  ], budget);
  if (f.commentsError) lines.push(`(inline review comments could not be read: ${f.commentsError})`);
  return lines.length ? `Open review feedback:\n${lines.join('\n')}` : '';
}

const OWN_BUDGET = 12_000;
const indent = (s: string) => s.trim().split('\n').map((l) => `  ${l}`).join('\n');

function renderThread(t: OwnThread): string {
  const where = `${t.path}${t.line ? `:${t.line}` : ''}`;
  const state = t.outdated ? 'the commented code has changed since' : 'the commented line is unchanged in the current diff';
  const replies = t.replies.map((r) => `- reply by ${r.author} at ${r.at}:\n${indent(r.body)}`);
  if (t.repliesCut) replies.unshift(`- (${t.repliesCut} older replies not shown)`);
  return [`### ${where}, commented on ${t.commit.slice(0, 8)} at ${t.at}; ${state}`, t.body.trim(), ...(replies.length ? replies : ['- no replies'])].join('\n');
}

/** The reviewer's own earlier inline comments and their replies, for a re-review; empty on a first review. */
export function renderOwnThreads(f: ReviewFeedback | undefined, budget = OWN_BUDGET): string {
  if (f?.ownThreadsError) return `(your earlier inline comments could not be read: ${f.ownThreadsError})`;
  if (!f?.ownThreads?.length) return '';
  const cut = f.ownThreadsCut ? [`(${f.ownThreadsCut} older comments of yours not shown)`] : [];
  const head = `Your earlier inline comments on this pull request, newest first: up to ${MAX_OWN_THREADS}, each with its newest ${MAX_THREAD_REPLIES} replies.`;
  return [head, ...fit(f.ownThreads.map(renderThread), budget, 3_000), ...cut].join('\n\n');
}

import type { GithubApp } from './app.ts';

/** The review feedback still open on a pull request: what a person or another session asked for, not yet answered. */
export type ReviewFeedback = {
  reviews: Array<{ author: string; state: 'CHANGES_REQUESTED' | 'COMMENTED'; commit: string; /** The review was made on the current head. */ current: boolean; body: string }>;
  /** First comment of each unresolved review thread. */
  comments: Array<{ author: string; path: string; line: number | null; body: string }>;
  /** Why the threads could not be read; the comments above are then incomplete. */
  commentsError?: string;
};

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

const MAX_REVIEW_PAGES = 30;

// Every page, since the list is oldest first and the newest review is the one that counts. shortcut: 3,000 reviews.
async function allReviews(app: GithubApp, owner: string, repo: string, base: string): Promise<any[]> {
  const all: any[] = [];
  for (let page = 1; page <= MAX_REVIEW_PAGES; page++) {
    const batch = (await app.asInstallation(owner, repo, 'GET', `${base}/reviews?per_page=100&page=${page}`)).json ?? [];
    all.push(...batch);
    if (batch.length < 100) break;
  }
  return all;
}

export async function reviewFeedback(app: GithubApp, owner: string, repo: string, number: number, headSha: string): Promise<ReviewFeedback> {
  const base = `/repos/${owner}/${repo}/pulls/${number}`;
  const found: ReviewFeedback = { reviews: openReviews(await allReviews(app, owner, repo, base), headSha), comments: [] };
  try {
    found.comments = await openComments(app, owner, repo, number);
  } catch (err) {
    found.commentsError = (err as Error).message.slice(0, 200);
  }
  return found;
}

const BUDGET = 6_000;
const EACH = 1_500;
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)} [cut ${s.length - n} chars]` : s);

/** The feedback as text for the implementer, within a total character budget; what does not fit is counted, not dropped silently. */
export function renderFeedback(f: ReviewFeedback | undefined, budget = BUDGET): string {
  if (!f) return '';
  const items = [
    ...f.reviews.map((r) => `- ${r.author} ${r.state} on ${r.commit.slice(0, 7)} (${r.current ? 'current head' : 'an older head'})${r.body.trim() ? `:\n${r.body.trim()}` : ''}`),
    ...f.comments.map((c) => `- ${c.author} on ${c.path}${c.line ? `:${c.line}` : ''}:\n${c.body.trim()}`),
  ];
  const shown: string[] = [];
  let used = 0;
  for (const item of items) {
    const text = clip(item, EACH);
    if (used + text.length > budget) break;
    shown.push(text);
    used += text.length;
  }
  const lines = [...shown, ...(shown.length < items.length ? [`(${items.length - shown.length} more review items cut to fit the size budget; read them on the pull request)`] : [])];
  if (f.commentsError) lines.push(`(inline review comments could not be read: ${f.commentsError})`);
  return lines.length ? `Open review feedback:\n${lines.join('\n')}` : '';
}

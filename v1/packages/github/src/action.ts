import { Type } from '@earendil-works/pi-ai';
import { submitToSession, type Action } from '@quack/sdk';

const params = Type.Object({ url: Type.String({ description: 'The pull request URL, https://github.com/owner/repo/pull/N' }) });

function parse(url: string): { owner: string; repo: string; number: string } {
  const u = new URL(url);
  const m = /^\/([^/]+)\/([^/]+)\/pull\/(\d+)\/?$/.exec(u.pathname);
  if (u.hostname.toLowerCase() !== 'github.com' || !m) throw new Error(`not a github.com pull request URL: ${url}`);
  return { owner: m[1], repo: m[2], number: m[3] };
}

/** Starts the reviewer as the review label does, under the same session key, so a label later joins this session instead of opening another. */
export const reviewPullRequest: Action<typeof params> = {
  name: 'review_pull_request',
  summary: 'Review a GitHub pull request with the code reviewer',
  params,
  async start({ url }, ctx) {
    const { owner, repo, number } = parse(url);
    const session = `github:${owner}/${repo}#${number}`;
    const title = `Review ${owner}/${repo}#${number}`;
    const res = await submitToSession(ctx.distributorUrl, {
      key: session, agent: 'reviewer', title, requestId: `home-review:${session}`,
      input: `Review pull request ${owner}/${repo}#${number} (requested from home).\nURL: ${url}`,
    }, ctx.fetch);
    if (!res.ok) throw new Error(`the distributor refused the review: HTTP ${res.status}`);
    return { session, title };
  },
};

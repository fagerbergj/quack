import type { GithubApp } from './app.ts';
import { allReviews } from './feedback.ts';

/** Why a review input needs no run, or undefined when it does; `finished` says whether the PR is merged or closed. A failed read starts the run rather than losing a review. */
export async function staleReason(app: GithubApp, t: { owner: string; repo: string; number: number; head?: string }): Promise<{ why: string; finished?: 'merged' | 'closed' } | undefined> {
  try {
    const path = `/repos/${t.owner}/${t.repo}/pulls/${t.number}`;
    const pr = (await app.asInstallation(t.owner, t.repo, 'GET', path)).json;
    if (pr?.state === 'closed') return { why: `the pull request is ${pr.merged ? 'merged' : 'closed'}`, finished: pr.merged ? 'merged' : 'closed' };
    if (!t.head) return undefined;
    const mine = `${await app.slug()}[bot]`.toLowerCase();
    // A dismissed review no longer stands, so its head is worth another look.
    const last = (await allReviews(app, t.owner, t.repo, path))
      .filter((v) => String(v.user?.login).toLowerCase() === mine && v.state !== 'DISMISSED' && v.state !== 'PENDING')
      .sort((a, b) => String(a.submitted_at).localeCompare(String(b.submitted_at))).at(-1);
    return last?.commit_id === t.head ? { why: `its last posted review already covers ${t.head}` } : undefined;
  } catch (err) {
    console.warn(`github: could not check whether ${t.owner}/${t.repo}#${t.number} needs a review: ${String(err)}`);
    return undefined;
  }
}

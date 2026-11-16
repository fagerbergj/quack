import type { GithubApp } from './app.ts';
import { allReviews } from './feedback.ts';

/** Why a review input needs no run, or undefined when it does; a failed read starts the run rather than losing a review. */
export async function staleReason(app: GithubApp, t: { owner: string; repo: string; number: number; head?: string }): Promise<string | undefined> {
  try {
    const path = `/repos/${t.owner}/${t.repo}/pulls/${t.number}`;
    if ((await app.asInstallation(t.owner, t.repo, 'GET', path)).json?.state === 'closed') return 'the pull request is closed or merged';
    if (!t.head) return undefined;
    const mine = `${await app.slug()}[bot]`.toLowerCase();
    // A dismissed review no longer stands, so its head is worth another look.
    const last = (await allReviews(app, t.owner, t.repo, path))
      .filter((v) => String(v.user?.login).toLowerCase() === mine && v.state !== 'DISMISSED' && v.state !== 'PENDING')
      .sort((a, b) => String(a.submitted_at).localeCompare(String(b.submitted_at))).at(-1);
    return last?.commit_id === t.head ? `its last posted review already covers ${t.head}` : undefined;
  } catch (err) {
    console.warn(`github: could not check whether ${t.owner}/${t.repo}#${t.number} needs a review: ${String(err)}`);
    return undefined;
  }
}

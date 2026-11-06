import { request } from 'node:http';

export type RunOutcome = { status?: string; reason?: string; detail?: unknown };

// Without an injected fetch, node:http, not fetch: undici gives up on headers after 300 s, and a wait:true invocation sends none until its run settles.
export async function settled(url: string, body: unknown, doFetch?: typeof fetch): Promise<RunOutcome> {
  if (doFetch) return (await doFetch(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })).json();
  return new Promise((resolve, reject) => {
    const req = request(url, { method: 'POST', headers: { 'content-type': 'application/json' } }, (res) => {
      let text = '';
      res.on('data', (c) => (text += c)).on('error', reject).on('end', () => {
        try { resolve(JSON.parse(text)); } catch (err) { reject(err); }
      });
    });
    req.on('error', reject).end(JSON.stringify(body));
  });
}

const RETRY = 'Comment `/review` to try again.';
const short = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v)).replace(/\s+/g, ' ').slice(0, 200);

/** What to tell the PR when a run ended without a review; undefined when it delivered one or the outcome is not final. */
export function noticeFor(outcome: RunOutcome, delivered: boolean): string | undefined {
  if (delivered) return undefined;
  if (outcome.status === 'unanswered') return `The review could not be completed (${outcome.reason ?? 'unknown'}${outcome.detail === undefined ? '' : `: ${short(outcome.detail)}`}). ${RETRY}`;
  if (outcome.status === 'done') return `The review ended without a review being posted. ${RETRY}`;
  return undefined;
}

/** Said on the PR (or issue) when the distributor refuses a `/fix` (`/implement`), with the distributor's own reason. */
export const fixRefused = (reason: string, issue = false) => `Quack could not start \`${issue ? '/implement' : '/fix'}\` on this ${issue ? 'issue' : 'pull request'}: ${reason}.`;

/** Said on the PR when a `/fix` arrives while the session is mid-run: it starts when that run ends. */
export const fixQueued = 'Quack is still working on this pull request, so `/fix` is queued and starts when the running agent finishes.';

/** Said on the PR when the distributor refuses a review for any reason but a busy session. */
export const reviewRefused = (reason: string) => `Quack could not start a review of this pull request: ${reason}. ${RETRY}`;

/** Said on the PR when a review arrives while another agent runs or waits on a person; a later push or `/review` replaces it. */
export const reviewQueued = 'Quack is busy on this pull request (another agent is running or waiting on a person), so the review is queued and starts when that finishes.';

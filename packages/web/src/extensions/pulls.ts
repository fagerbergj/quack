import { json } from '../api.ts';

/** GitHub's pull request, cut to the subset the github app mirrors: https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request */
export type Pull = { number: number; state: 'open' | 'closed'; merged: boolean; merged_at: string | null; closed_at: string | null; html_url: string; title: string };
/** GitHub's pull_request webhook event as the app's /events stream relays it: https://docs.github.com/en/webhooks/webhook-events-and-payloads#pull_request */
export type PullEvent = { action: string; repository: { full_name: string }; pull_request: Pull };

const KEY = /^github:([^/]+\/[^#]+)#(\d+)$/;

/** owner/repo#n, lowercased because GitHub matches owner and repo in any case; undefined for a key that is not a github PR. */
export const pullKey = (sessionKey: string) => {
  const m = KEY.exec(sessionKey);
  return m ? `${m[1].toLowerCase()}#${m[2]}` : undefined;
};
export const eventKey = (e: PullEvent) => `${e.repository.full_name.toLowerCase()}#${e.pull_request.number}`;

/** The PR behind a pullKey from the app's GitHub mirror; undefined when the app, its installation or the PR is not there. */
export async function readPull(key: string): Promise<Pull | undefined> {
  const [repo, n] = key.split('#');
  const res = await fetch(`apps/github/repos/${repo}/pulls/${n}`);
  return res.status === 404 ? undefined : json<Pull>(res);
}

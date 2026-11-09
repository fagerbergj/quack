import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { GithubApp } from './app.ts';

/** A subset of GitHub's pull request object, with its names and types: https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request */
export type Pull = { number: number; state: 'open' | 'closed'; merged: boolean; merged_at: string | null; closed_at: string | null; html_url: string; title: string };
/** GitHub's `pull_request` webhook event, cut to what the badges use: https://docs.github.com/en/webhooks/webhook-events-and-payloads#pull_request */
export type PullEvent = { action: string; repository: { full_name: string }; pull_request: Pull };
type Entry = Pull & { checked: number };

const OPEN_TTL_MS = 10 * 60_000;
const KEEP_FINAL_MS = 30 * 24 * 3600_000;
const FIELDS = ['number', 'state', 'merged', 'merged_at', 'closed_at', 'html_url', 'title'] as const;

/** GET /repos/{owner}/{repo}/pulls/{pull_number}, in GitHub's characters for owner and repo names. */
export const MIRROR_PATH = /^\/repos\/([\w.-]+)\/([\w.-]+)\/pulls\/(\d+)$/;

const keyOf = (fullName: string, number: number) => `${fullName.toLowerCase()}#${number}`;

/** The mirrored subset of a GitHub pull request, from the REST response or a webhook payload. */
export const pullOf = (pr: Record<string, any>): Pull => ({
  number: pr.number, state: pr.state === 'closed' ? 'closed' : 'open', merged: pr.merged === true,
  merged_at: pr.merged_at ?? null, closed_at: pr.closed_at ?? null, html_url: pr.html_url, title: pr.title,
});

// GitHub's own action names: a state change is closed or reopened, any other change to the subset is an edit.
function actionFor(before: Entry | undefined, after: Pull): string {
  if (before?.state === after.state) return 'edited';
  if (after.state === 'closed') return 'closed';
  return before ? 'reopened' : 'opened';
}

/** PRs as GitHub last reported them, kept across restarts when given a file; closed is final, open is re-read after a TTL. Every change is published to `subscribe`rs. */
export function pullStates(file?: string, now: () => number = Date.now) {
  let entries: { [key: string]: Entry } = {};
  try {
    if (file) entries = JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    // First run or an unreadable file: PRs are re-read lazily.
  }
  const listeners = new Set<(e: PullEvent) => void>();
  const save = () => {
    // A dropped closed entry is read again if a page still asks.
    for (const [k, e] of Object.entries(entries)) if (e.state !== 'open' && now() - e.checked > KEEP_FINAL_MS) delete entries[k];
    if (!file) return;
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(`${file}.tmp`, JSON.stringify(entries));
    renameSync(`${file}.tmp`, file);
  };
  // An entry from before the mirror lacks html_url, so it is read again.
  const fresh = (e: Entry | undefined): e is Entry => !!e?.html_url && (e.state !== 'open' || now() - e.checked < OPEN_TTL_MS);

  /** The one write path: webhooks and re-reads both land here, so no change skips the event. */
  function write(fullName: string, pull: Pull, action?: string) {
    const key = keyOf(fullName, pull.number);
    const before = entries[key];
    entries[key] = { ...pull, checked: now() };
    save();
    if (before && FIELDS.every((f) => before[f] === pull[f])) return;
    const event = { action: action ?? actionFor(before, pull), repository: { full_name: fullName }, pull_request: pull };
    for (const listener of listeners) listener(event);
  }

  return {
    write,
    /** The PR, from the store while fresh, else from GitHub; undefined when GitHub has no such PR. Throws NoInstallation or GithubError. */
    async read(app: GithubApp, owner: string, repo: string, number: number): Promise<Pull | undefined> {
      const e = entries[keyOf(`${owner}/${repo}`, number)];
      if (fresh(e)) return pullOf(e);
      const pr = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/pulls/${number}`)).json;
      if (!pr) return undefined;
      const pull = pullOf(pr);
      write(pr.base?.repo?.full_name ?? `${owner}/${repo}`, pull);
      return pull;
    },
    subscribe(listener: (e: PullEvent) => void) {
      listeners.add(listener);
      return () => void listeners.delete(listener);
    },
  };
}

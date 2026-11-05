import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { conventions, git, remoteFor, type SessionConfig } from './checkout.ts';

export type IssueRecord = { owner: string; repo: string; number: number; title: string; body: string; author: string | null; state: string; labels: string[]; baseRef: string };

/** The issue being implemented and the branch cut for it; baseSha is where commits start, as a PR's head sha is for /fix. */
export type IssueWork = { issue: IssueRecord; branch: string; baseSha: string };

export const IssueDoc = defineDoc<{ work: IssueWork | null }>({
  kind: 'github.issue', version: 1, scope: 'conversation', history: 'latest', fork: 'current', initial: () => ({ work: null }), checkpointWhen: () => false,
});

/** quack/<n>-<slug of the title>; the number alone when the title has no usable words. */
export function branchFor(number: number, title: string): string {
  const slug = title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+/, '').slice(0, 40).replace(/-+$/, '');
  return slug ? `quack/${number}-${slug}` : `quack/${number}`;
}

/** undefined when the number is not an issue (the caller then reports the failure it already had). */
export async function fetchIssue(config: SessionConfig, owner: string, repo: string, number: number): Promise<IssueRecord | undefined> {
  const res = await fetch(`${config.serviceUrl}/issues/${owner}/${repo}/${number}`, { headers: { authorization: `Bearer ${config.serviceToken}` } });
  if (res.status === 404) return undefined;
  if (!res.ok) throw new Error(`cannot read issue ${owner}/${repo}#${number}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json() as Promise<IssueRecord>;
}

/** Clones the base branch (the repository's default unless the deployment's issueBase lists the repository) and cuts the work branch from it; returns the facts the task needs. */
export async function checkoutIssue(api: ToolExecutionApi, config: SessionConfig, dir: string, issue: IssueRecord, ctx: Context) {
  const remote = remoteFor(config, issue.owner, issue.repo);
  const run = (...args: string[]) => git(api, args, config, remote, ctx);
  if (!(await api.env!.readTextFile(`${dir}/.git/HEAD`, ctx)).ok) await run('clone', '--filter=blob:none', '--no-checkout', remote, dir);
  await run('-C', dir, 'fetch', '--filter=blob:none', 'origin', issue.baseRef);
  const branch = branchFor(issue.number, issue.title);
  await run('-C', dir, 'checkout', '-B', branch, `origin/${issue.baseRef}`);
  const baseSha = (await run('-C', dir, 'rev-parse', 'HEAD')).trim();
  await api.commit(async (tx) => { (await tx.doc(IssueDoc, api.conversationId)).work = { issue, branch, baseSha }; }, ctx);
  return [
    `${issue.owner}/${issue.repo}#${issue.number} by ${issue.author}: ${issue.title}`,
    `Created branch ${branch} from ${issue.baseRef} at ${baseSha} in ./${dir}; commit on it, then call deliver, which opens the pull request.`,
    `Labels: ${issue.labels.join(', ') || 'none'}\nIssue:\n${issue.body || '(no description)'}`,
    await conventions(api, dir, ctx),
  ].join('\n\n');
}

/** Non-empty when the branch already exists on the remote at a commit other than sha. */
export async function branchTaken(api: ToolExecutionApi, config: SessionConfig, dir: string, w: IssueWork, sha: string, ctx: Context): Promise<string> {
  const remote = remoteFor(config, w.issue.owner, w.issue.repo);
  const listed = (await git(api, ['-C', dir, 'ls-remote', 'origin', `refs/heads/${w.branch}`], config, remote, ctx)).trim().split(/\s/)[0];
  return listed && listed !== sha ? `the branch ${w.branch} already exists on ${w.issue.owner}/${w.issue.repo} with other commits; nothing was pushed` : '';
}

// pr.md's first line is the title; the rest is the body.
const bodyOf = (prMd: string) => prMd.trim().split('\n').slice(1).join('\n').trim();

/** Opens the pull request through the service, which holds the token; the body closes the issue. */
export async function openPull(config: SessionConfig, w: IssueWork, title: string, body: string): Promise<string> {
  const text = `${bodyOf(body)}\n\nCloses #${w.issue.number}`.trim();
  const res = await fetch(`${config.serviceUrl}/pulls`, {
    method: 'POST', headers: { authorization: `Bearer ${config.serviceToken}` },
    body: JSON.stringify({ owner: w.issue.owner, repo: w.issue.repo, head: w.branch, base: w.issue.baseRef, title, body: text }),
  });
  if (!res.ok) throw new Error(`cannot open the pull request: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return ((await res.json()) as { url: string }).url;
}

/** Sets an open pull request's title and body to the pr.md the approver saw; closing lines the live body had and pr.md lacks stay. */
export async function editPull(config: SessionConfig, pr: { owner: string; repo: string; number: number }, title: string, prMd: string, liveBody: string | undefined): Promise<void> {
  const text = bodyOf(prMd);
  const kept = (liveBody?.match(/^(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?) #\d+$/gim) ?? []).filter((l) => !text.includes(l));
  const res = await fetch(`${config.serviceUrl}/pulls/${pr.owner}/${pr.repo}/${pr.number}`, {
    method: 'PATCH', headers: { authorization: `Bearer ${config.serviceToken}` },
    body: JSON.stringify({ title, body: [text, ...kept].join('\n\n') }),
  });
  if (!res.ok) throw new Error(`${res.status} ${(await res.text()).slice(0, 200)}`);
}

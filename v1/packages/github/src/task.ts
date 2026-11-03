import type { Context } from '@earendil-works/chord/context';
import { int, obj, str } from '@earendil-works/pi-schema';
import { defineTool, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { Artifacts, attachLink, gated, seal, type ArtifactKind } from '@quack/sdk';
import { conventions, fetchRecord, git, prFacts, PullRequestDoc, remoteFor, syncTree, type PullRequestRecord, type SessionConfig } from './checkout.ts';
import { sessionRuns, unbackedClaims } from './claims.ts';
import { parseReview } from './review.ts';

const CONVENTIONAL = /^[a-z]+(\([^)]+\))?!?: \S/;
const PR_BODY = 'artifacts/pr.md';

const titleOf = (text: string) => text.trim().split('\n')[0].replace(/^#+\s*/, '');

/** The pull request text the implementer writes: a title line, then the body. The human approver reads it before the push. */
export const prBodyKind: ArtifactKind = {
  kind: 'github.pr_body', path: PR_BODY,
  validate: (text) => (text.trim() ? [] : ['is empty; start with a title line, then what changed, why and how you verified it']),
  summarize: titleOf,
};

const reply = (text: string, isError = false) => ({ output: [{ type: 'text' as const, text }], isError });

// The newest quack review in this session, so a /fix after a review sees what it asked for.
async function lastReview(api: ToolExecutionApi, ctx: Context): Promise<string> {
  const last = (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds['github.review']?.versions.at(-1);
  if (!last) return '';
  const findings = parseReview(last.content).findings.map((f) => `- ${f.label}${f.path ? ` ${f.path}:${f.line}` : ''}: ${f.text}`);
  return `Latest quack review (${last.outcome ?? 'no verdict'}):\n${findings.join('\n') || '(no findings)'}`;
}

const refusal = (pr: PullRequestRecord) => {
  if (pr.state !== 'open') return `the pull request is ${pr.state}; nothing to fix`;
  if (pr.headRepo && pr.headRepo.toLowerCase() !== `${pr.owner}/${pr.repo}`.toLowerCase()) return `the head branch lives in ${pr.headRepo}, a fork; the GitHub App cannot push there, so ask the author to apply the change`;
  return '';
};

/** Checks out a pull request's head branch (not detached) so commits extend it, and returns what the task needs. */
export function checkoutTaskTool(config: SessionConfig) {
  const dir = config.repoDir ?? 'repo';
  return defineTool({
    name: 'checkout_task',
    description: `Check out the head branch of a pull request into ./${dir} and return its description, CI checks, the latest review's findings, linked issues and the repo's conventions. Call it first.`,
    parameters: obj({ repository: str({ description: 'owner/repo' }), number: int({ minimum: 1 }) }, { open: true }),
    replay: 'safe',
    async execute(args, api, ctx) {
      const [owner, repo] = args.repository.split('/');
      if (!owner || !repo) return reply('repository must be owner/repo', true);
      if (!api.env) return reply('this session has no workspace', true);
      const pr = await fetchRecord(config, owner, repo, args.number);
      const refused = refusal(pr);
      if (refused) return reply(`Nothing checked out: ${refused}.`, true);
      await syncTree(api, dir, pr, config, remoteFor(config, owner, repo), ctx, pr.headRef);
      await api.commit(async (tx) => { (await tx.doc(PullRequestDoc, api.conversationId)).pr = JSON.parse(JSON.stringify(pr)); }, ctx);
      const text = [
        `${owner}/${repo}#${pr.number} by ${pr.author}: ${pr.title}`,
        `Checked out branch ${pr.headRef} at ${pr.headSha} in ./${dir}; commit on it, then call deliver.`,
        ...prFacts(pr),
        await lastReview(api, ctx),
        await conventions(api, dir, ctx),
      ].filter(Boolean).join('\n\n');
      return reply(text);
    },
  });
}

type Repo = { api: ToolExecutionApi; config: SessionConfig; remote: string; dir: string; ctx: Context };
const out = (r: Repo, ...args: string[]) => git(r.api, ['-C', r.dir, ...args], r.config, r.remote, r.ctx).then((s) => s.trim());

// Code checks on the commits made since checkout; every failure is named so one fix pass covers them.
async function problems(r: Repo, pr: PullRequestRecord): Promise<string[]> {
  const subjects = (await out(r, 'log', '--format=%s', `${pr.headSha}..HEAD`)).split('\n').filter(Boolean);
  const found = subjects.filter((s) => !CONVENTIONAL.test(s)).map((s) => `commit subject is not Conventional Commits "type(scope): subject": ${s}`);
  if (!subjects.length) found.push('there is no new commit since checkout');
  if (await out(r, 'status', '--porcelain')) found.push('the working tree has uncommitted changes; commit or discard them');
  const branch = await out(r, 'branch', '--show-current');
  if (branch !== pr.headRef) found.push(`HEAD is on "${branch || 'a detached head'}", not the pull request branch ${pr.headRef}`);
  return found;
}

// Runs once the approver said yes: the push goes out only if the remote head is still the one checked out.
function pushTool(config: SessionConfig, dir: string): ToolRegistration {
  return gated(defineTool({
    name: 'deliver',
    description: 'deliver',
    parameters: obj({ summary: str(), pullRequest: str(), version: int(), title: str(), body: str(), commits: str(), diffStat: str() }, { open: true }),
    async execute(args, api, ctx) {
      const pr = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr;
      if (!pr) return reply('no pull request is checked out; call checkout_task first', true);
      const live = await fetchRecord(config, pr.owner, pr.repo, pr.number);
      if (live.headSha !== pr.headSha) return reply(`Nothing was pushed: ${pr.headRef} moved from ${pr.headSha} to ${live.headSha} since checkout; call checkout_task again and redo the change on the new head.`, true);
      const r = { api, config, remote: remoteFor(config, pr.owner, pr.repo), dir, ctx };
      const sha = await out(r, 'rev-parse', 'HEAD');
      await git(api, ['-C', dir, 'push', 'origin', `HEAD:refs/heads/${pr.headRef}`], config, r.remote, ctx);
      await api.commit(async (tx) => { (await tx.doc(PullRequestDoc, api.conversationId)).pr!.headSha = sha; }, ctx);
      const url = `https://github.com/${pr.owner}/${pr.repo}/pull/${pr.number}`;
      await attachLink(api, prBodyKind.kind, args.version, { label: 'Pushed', url }, ctx);
      return reply(`Pushed ${sha} to ${pr.headRef}: ${url}`);
    },
  }) as ToolRegistration);
}

/** Checks the delivery in code, shows the human approver the text, commits and diffstat, then pushes after approval. */
export function deliverTool(config: SessionConfig) {
  const dir = config.repoDir ?? 'repo';
  const push = pushTool(config, dir);
  return defineTool({
    name: 'deliver',
    description: `Deliver your commits to the pull request branch. Write ${PR_BODY} first: a title line, then what changed, why and how you verified it. It checks the commits and tree, asks a person to approve, then pushes. It returns the checks that failed so you can fix them and call it again.`,
    parameters: obj({}, { open: true }),
    replay: 'safe',
    async execute(_args, api, ctx) {
      const pr = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr;
      if (!pr) return reply('Nothing to deliver: no pull request is checked out. Delivering from an issue (opening a pull request) is not supported yet; call checkout_task with a pull request.', true);
      const r = { api, config, remote: remoteFor(config, pr.owner, pr.repo), dir, ctx };
      const found = await problems(r, pr);
      const text = await api.env!.readTextFile(PR_BODY, ctx);
      if (!text.ok) found.push(`${PR_BODY} cannot be read; write it first`);
      else if (!text.value.trim()) found.push(`${PR_BODY} is empty`);
      if (text.ok) found.push(...unbackedClaims(text.value, await sessionRuns(api, ctx), (await out(r, 'log', '--format=%H', `${pr.headSha}..HEAD`)).split('\n')));
      if (found.length) return reply(`Not delivered; fix these and call deliver again:\n- ${found.join('\n- ')}`, true);
      const sealed = await seal(api, prBodyKind, ctx);
      const commits = await out(r, 'log', '--format=%h %s', `${pr.headSha}..HEAD`);
      const diffStat = await out(r, 'diff', '--stat', `${pr.headSha}..HEAD`);
      const title = titleOf(sealed.content);
      const summary = `Push ${commits.split('\n').length} commit(s) to ${pr.headRef} of ${pr.owner}/${pr.repo}#${pr.number}: ${title}`;
      return push.execute({ summary, pullRequest: `${pr.owner}/${pr.repo}#${pr.number}`, version: sealed.version, title, body: sealed.content, commits, diffStat } as never, api, ctx);
    },
  }) as ToolRegistration;
}

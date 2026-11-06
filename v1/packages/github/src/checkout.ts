
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import type { Context } from '@earendil-works/chord/context';
import { int, obj, opt, str } from '@earendil-works/pi-schema';
import { defineDoc, defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { Artifacts, type SealedArtifact } from '@quack/sdk';
import type { ReviewFeedback } from './feedback.ts';

export type SessionConfig = { serviceUrl: string; serviceToken: string; repoDir?: string; /** Where repositories are cloned from; tests use a local directory. */ gitBaseUrl?: string };

export type PullRequestRecord = {
  owner: string; repo: string; number: number; title: string; body: string; author: string | null;
  /** open, closed or merged; a closed or merged PR is not reviewed. */
  state: 'open' | 'closed' | 'merged';
  headSha: string; headRef: string; baseRef: string;
  /** Pins the base, as a replay does: only it and the head are fetched, so later history, merges and tags stay out of the clone. */
  baseSha?: string;
  /** owner/name of the head branch's repository; a fork's head cannot be pushed to by the App. Absent in older records. */
  headRepo?: string;
  checks: Array<{ name: string; status: string; conclusion: string | null }>;
  linkedIssues: Array<{ number: number; title: string; body: string }>;
  /** Open reviews and unresolved comments; absent from older records and from the opening-card read. */
  feedback?: ReviewFeedback;
};

/** reviewed: the head each version was first judged under, so a version never posts against a later checkout. posted: versions of the review already sent, so none goes out twice; postedThisCheckout: the one a run sent. */
export const PullRequestDoc = defineDoc<{ pr: PullRequestRecord | null; posted?: number[]; postedThisCheckout?: number; reviewed?: Record<string, string>; checkout?: { head: string; text: string }; /** Why the last checkout found nothing to review; submit_review refuses while it stands. */ skipped?: string }>({
  kind: 'github.pr', version: 1, scope: 'conversation', history: 'latest', fork: 'current', initial: () => ({ pr: null }), checkpointWhen: () => false,
});

const ASKPASS = fileURLToPath(new URL('./askpass.mjs', import.meta.url));
const CONVENTIONS = ['AGENTS.md', 'CLAUDE.md'];

async function run(api: ToolExecutionApi, label: string, argv: string[], config: SessionConfig, remote: string, ctx: Context) {
  let output = '';
  // The service secret reaches only this git child; credential helpers are off so nothing is stored (D56).
  const env = { GIT_ASKPASS: ASKPASS, GIT_TERMINAL_PROMPT: '0', QUACK_GITHUB_URL: config.serviceUrl, QUACK_GITHUB_TOKEN: config.serviceToken, QUACK_GH_REMOTE: remote };
  const result = await api.env!.exec(argv, { env, onOutput: (t) => { output += t; } }, ctx);
  if (!result.ok || result.value.exitCode !== 0) throw new Error(`git ${label} failed: ${output.slice(-500)}`);
  return output;
}

export const git = (api: ToolExecutionApi, args: string[], config: SessionConfig, remote: string, ctx: Context) => run(api, args[0], ['git', '-c', 'credential.helper=', ...args], config, remote, ctx);

export const remoteFor = (config: SessionConfig, owner: string, repo: string) => `${config.gitBaseUrl ?? 'https://github.com'}/${owner}/${repo}.git`;

/** undefined when the number is not a pull request (a 404), as an issue number is. */
export async function findRecord(config: SessionConfig, owner: string, repo: string, number: number): Promise<PullRequestRecord | undefined> {
  const res = await fetch(`${config.serviceUrl}/pulls/${owner}/${repo}/${number}`, { headers: { authorization: `Bearer ${config.serviceToken}` } });
  if (res.status === 404) return undefined;
  if (!res.ok) throw new Error(`cannot read ${owner}/${repo}#${number}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json() as Promise<PullRequestRecord>;
}

/** The pull request's live head and description: one GitHub read, where a full record also reads its checks, reviews and issues. */
export async function liveHead(config: SessionConfig, owner: string, repo: string, number: number): Promise<{ headSha: string; body: string }> {
  const res = await fetch(`${config.serviceUrl}/pulls/${owner}/${repo}/${number}/head`, { headers: { authorization: `Bearer ${config.serviceToken}` } });
  if (!res.ok) throw new Error(`cannot read the head of ${owner}/${repo}#${number}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json() as Promise<{ headSha: string; body: string }>;
}

export async function fetchRecord(config: SessionConfig, owner: string, repo: string, number: number): Promise<PullRequestRecord> {
  const pr = await findRecord(config, owner, repo, number);
  if (!pr) throw new Error(`cannot read ${owner}/${repo}#${number}: 404 no such pull request`);
  return pr;
}

const alreadyPosted = (doc: { posted?: number[]; reviewed?: Record<string, string> } | undefined, sha: string) => !!doc?.posted?.some((v) => doc.reviewed?.[String(v)] === sha);
// What a review read besides the code: a new description or CI result is worth another look at the same head.
const seen = (pr: PullRequestRecord | null | undefined) => JSON.stringify(pr && [pr.headSha, pr.body, pr.checks]);
const skipReason = (pr: PullRequestRecord, prior: (Parameters<typeof alreadyPosted>[0] & { pr?: PullRequestRecord | null }) | undefined) =>
  pr.state !== 'open' ? 'the pull request is closed; nothing to do' : alreadyPosted(prior, pr.headSha) && seen(prior?.pr) === seen(pr) ? `already reviewed ${pr.headSha}: nothing to do this turn` : '';
const staleNote = (asked: string | undefined, live: string) => (asked && asked !== live ? `The request was for ${asked}, an older head; this reviews the live head ${live}.` : '');

async function atHead(api: ToolExecutionApi, dir: string, sha: string, config: SessionConfig, remote: string, ctx: Context): Promise<boolean> {
  if (!(await api.env!.readTextFile(`${dir}/.git/HEAD`, ctx)).ok) return false;
  return (await git(api, ['-C', dir, 'rev-parse', 'HEAD'], config, remote, ctx).catch(() => '')).trim() === sha;
}

// With `branch`, the head is checked out on that branch so commits extend it; otherwise detached.
export async function syncTree(api: ToolExecutionApi, dir: string, pr: PullRequestRecord, config: SessionConfig, remote: string, ctx: Context, branch?: string) {
  const cloned = (await api.env!.readTextFile(`${dir}/.git/HEAD`, ctx)).ok;
  if (!cloned && pr.baseSha) await git(api, ['init', '-q', dir], config, remote, ctx);
  if (!cloned && pr.baseSha) await git(api, ['-C', dir, 'remote', 'add', 'origin', remote], config, remote, ctx);
  if (!cloned && !pr.baseSha) await git(api, ['clone', '--filter=blob:none', '--no-checkout', remote, dir], config, remote, ctx);
  const refs = pr.baseSha ? ['--no-tags', 'origin', `+${pr.baseSha}:refs/remotes/origin/${pr.baseRef}`, pr.headSha] : ['origin', pr.baseRef, `pull/${pr.number}/head`];
  await git(api, ['-C', dir, 'fetch', '--filter=blob:none', ...refs], config, remote, ctx);
  await git(api, ['-C', dir, 'checkout', ...(branch ? ['-B', branch] : ['--detach']), pr.headSha], config, remote, ctx);
}

export async function conventions(api: ToolExecutionApi, dir: string, ctx: Context): Promise<string> {
  for (const name of CONVENTIONS) {
    const read = await api.env!.readTextFile(`${dir}/${name}`, ctx);
    if (read.ok) return `${name}:\n${read.value.slice(0, 12_000)}`;
  }
  return 'No AGENTS.md or CLAUDE.md in the repository root.';
}

type Approved = { version: number; sha: string; link?: string };

// The newest review quack posted here, when it was an approval the judge passed (not one released at the revision limit).
async function lastApproval(api: ToolExecutionApi, prior: Awaited<ReturnType<typeof priorDoc>>, ctx: Context): Promise<Approved | undefined> {
  const kinds = (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds;
  const last = (kinds?.['github.review']?.versions ?? []).filter((v: SealedArtifact) => v.links?.length).at(-1);
  const sha = last && prior?.reviewed?.[String(last.version)];
  return last && sha && last.outcome === 'approve' && (last.verdict?.passed === true || last.carriedFrom) ? { version: last.version, sha, link: last.links![0].url } : undefined;
}
const priorDoc = (api: ToolExecutionApi, ctx: Context) => api.snapshot(PullRequestDoc, api.conversationId, ctx);

// Hashes the PR's change against its merge base with the base's current tip; `git patch-id --stable` ignores line numbers and whitespace-free context shifts a rebase causes.
async function patchId(api: ToolExecutionApi, dir: string, base: string, sha: string, config: SessionConfig, remote: string, ctx: Context): Promise<string> {
  const script = 'git -C "$1" diff "$2...$3" | git patch-id --stable';
  const id = (await run(api, 'patch-id', ['sh', '-c', script, 'sh', dir, `origin/${base}`, sha], config, remote, ctx)).trim().split(/\s+/)[0];
  if (!id) throw new Error(`empty diff for ${sha}`);
  return id;
}

const carriedBody = (a: Approved) => `Same change as the approved review of ${a.link ? `[${a.sha.slice(0, 8)}](${a.link})` : a.sha.slice(0, 8)} (rebase only); approval carried forward`;

// A push that only rebases an approved PR keeps its approval: the new head posts APPROVE and is recorded as a posted version, with no review turn. Any doubt returns undefined and the full review runs.
async function carryApproval(api: ToolExecutionApi, dir: string, pr: PullRequestRecord, config: SessionConfig, remote: string, ctx: Context): Promise<string | undefined> {
  try {
    const approved = await lastApproval(api, await priorDoc(api, ctx), ctx);
    if (!approved || approved.sha === pr.headSha) return undefined;
    await git(api, ['-C', dir, 'fetch', '--filter=blob:none', 'origin', approved.sha], config, remote, ctx);
    const [was, now] = await Promise.all([approved.sha, pr.headSha].map((sha) => patchId(api, dir, pr.baseRef, sha, config, remote, ctx)));
    if (was !== now) return undefined;
    // Like postCleared, refuse to approve a head that moved while the patch-ids ran.
    if ((await liveHead(config, pr.owner, pr.repo, pr.number)).headSha !== pr.headSha) return undefined;
    const body = carriedBody(approved);
    // The delivery id lets a rerun after a crash find this approval instead of posting it twice.
    const post = { owner: pr.owner, repo: pr.repo, number: pr.number, commitId: pr.headSha, event: 'APPROVE', body, deliveryId: `${api.conversationId}/carry/${pr.headSha}` };
    const res = await fetch(`${config.serviceUrl}/reviews`, { method: 'POST', body: JSON.stringify(post), headers: { authorization: `Bearer ${config.serviceToken}` } });
    if (!res.ok) return undefined;
    const url = ((await res.json()) as { url?: string }).url;
    await recordCarried(api, pr, body, url, approved, ctx);
    return `${body}. It is posted${url ? `: ${url}` : ''}. The review is done; do not review this head again.`;
  } catch {
    return undefined;
  }
}

async function recordCarried(api: ToolExecutionApi, pr: PullRequestRecord, body: string, url: string | undefined, from: Approved, ctx: Context) {
  const content = `## Takeaway\n${body}\n\n## Verdict\napprove\n\n## Findings\n`;
  await api.commit(async (tx) => {
    const kind = (await tx.doc(Artifacts, api.conversationId)).kinds;
    kind['github.review'] ??= { versions: [] };
    const versions = kind['github.review'].versions;
    const version = versions.length + 1;
    versions.push({ kind: 'github.review', version, hash: createHash('sha256').update(content).digest('hex'), content, sealedAt: api.now(), verdict: null, basis: pr.headSha, carriedFrom: { sha: from.sha, ...(from.link ? { url: from.link } : {}) }, outcome: 'approve', summary: 'Approve · carried forward', ...(url ? { links: [{ label: 'Posted', url }] } : {}) });
    const d = await tx.doc(PullRequestDoc, api.conversationId);
    d.reviewed = { ...d.reviewed, [String(version)]: pr.headSha };
    d.posted = [...(d.posted ?? []), version];
    d.postedThisCheckout = version;
  }, ctx);
}

/** What both roles read of a pull request besides the code: description, CI result and linked issues. */
export const prFacts = (pr: PullRequestRecord) => [
  `Description:\n${pr.body || '(none)'}`,
  `CI checks: ${pr.checks.map((c) => `${c.name}=${c.conclusion ?? c.status}`).join(', ') || 'none reported'}`,
  ...pr.linkedIssues.map((i) => `Linked issue #${i.number}: ${i.title}\n${i.body}`),
];

// Checks out the pull request's head with its base fetched for diffing, and records the PR for review and judging.
export function checkoutTool(config: SessionConfig) {
  const dir = config.repoDir ?? 'repo';
  return defineTool({
    name: 'checkout_pr',
    description: `Check out a pull request into ./${dir} at its head, with its base fetched, and return its description, CI checks, linked issues and the repo's conventions. Call it first.`,
    parameters: obj({ repository: str({ description: 'owner/repo' }), number: int({ minimum: 1 }), head: opt(str({ description: 'the head sha the request named, if any' })) }, { open: true }),
    replay: 'safe',
    async execute(args, api, ctx) {
      const [owner, repo] = args.repository.split('/');
      if (!owner || !repo) return { output: [{ type: 'text', text: 'repository must be owner/repo' }], isError: true };
      if (!api.env) return { output: [{ type: 'text', text: 'this session has no workspace' }], isError: true };
      const pr = await fetchRecord(config, owner, repo, args.number);
      const prior = await api.snapshot(PullRequestDoc, api.conversationId, ctx);
      const skip = skipReason(pr, prior);
      if (skip) {
        await api.commit(async (tx) => { (await tx.doc(PullRequestDoc, api.conversationId)).skipped = skip; }, ctx);
        return { output: [{ type: 'text', text: skip }] };
      }
      if (prior?.skipped) await api.commit(async (tx) => { delete (await tx.doc(PullRequestDoc, api.conversationId)).skipped; }, ctx);
      const remote = remoteFor(config, owner, repo);
      const stale = staleNote(args.head, pr.headSha);
      // The same head with its tree still there needs no clone or fetch.
      const reuse = prior?.checkout?.head === pr.headSha && seen(prior.pr) === seen(pr) && (await atHead(api, dir, pr.headSha, config, remote, ctx));
      if (reuse) return { output: [{ type: 'text', text: [stale, prior!.checkout!.text].filter(Boolean).join('\n\n') }] };
      await syncTree(api, dir, pr, config, remote, ctx);
      await api.commit(async (tx) => {
        const d = await tx.doc(PullRequestDoc, api.conversationId);
        d.pr = JSON.parse(JSON.stringify(pr));
        delete d.postedThisCheckout;
      }, ctx);
      const carried = await carryApproval(api, dir, pr, config, remote, ctx);
      if (carried) return { output: [{ type: 'text', text: [stale, carried].filter(Boolean).join('\n\n') }] };
      const stat = await git(api, ['-C', dir, 'diff', '--stat', `origin/${pr.baseRef}...${pr.headSha}`], config, remote, ctx);
      const text = [
        `${owner}/${repo}#${pr.number} by ${pr.author}: ${pr.title}`,
        `Checked out ${pr.headSha} in ./${dir}; diff against origin/${pr.baseRef}: git -C ${dir} diff origin/${pr.baseRef}...HEAD`,
        ...prFacts(pr),
        `Changed files:\n${stat}`,
        await conventions(api, dir, ctx),
      ].filter(Boolean).join('\n\n');
      await api.commit(async (tx) => { (await tx.doc(PullRequestDoc, api.conversationId)).checkout = { head: pr.headSha, text }; }, ctx);
      return { output: [{ type: 'text', text: [stale, text].filter(Boolean).join('\n\n') }] };
    },
  });
}


import { fileURLToPath } from 'node:url';
import type { Context } from '@earendil-works/chord/context';
import { Type } from '@earendil-works/pi-ai';
import { defineDoc, defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { blastRadius } from './blast.ts';

export type SessionConfig = { serviceUrl: string; serviceToken: string; repoDir?: string; /** Where repositories are cloned from; tests use a local directory. */ gitBaseUrl?: string };

export type PullRequestRecord = {
  owner: string; repo: string; number: number; title: string; body: string; author: string | null;
  headSha: string; headRef: string; baseRef: string;
  /** Pins the base, as a replay does: only it and the head are fetched, so later history, merges and tags stay out of the clone. */
  baseSha?: string;
  checks: Array<{ name: string; status: string; conclusion: string | null }>;
  linkedIssues: Array<{ number: number; title: string; body: string }>;
};

/** posted: versions of the review already sent, so none goes out twice; postedThisCheckout: the one a run sent. */
export const PullRequestDoc = defineDoc<{ pr: PullRequestRecord | null; posted?: number[]; postedThisCheckout?: number }>({
  kind: 'github.pr', version: 1, scope: 'conversation', history: 'latest', fork: 'current', initial: () => ({ pr: null }), checkpointWhen: () => false,
});

const ASKPASS = fileURLToPath(new URL('./askpass.mjs', import.meta.url));
const CONVENTIONS = ['AGENTS.md', 'CLAUDE.md'];

async function git(api: ToolExecutionApi, args: string[], config: SessionConfig, remote: string, ctx: Context) {
  let output = '';
  // The service secret reaches only this git child; credential helpers are off so nothing is stored (D56).
  const env = { GIT_ASKPASS: ASKPASS, GIT_TERMINAL_PROMPT: '0', QUACK_GH_SERVICE_URL: config.serviceUrl, QUACK_GH_SERVICE_TOKEN: config.serviceToken, QUACK_GH_REMOTE: remote };
  const result = await api.env!.exec(['git', '-c', 'credential.helper=', ...args], { env, onOutput: (t) => { output += t; } }, ctx);
  if (!result.ok || result.value.exitCode !== 0) throw new Error(`git ${args[0]} failed: ${output.slice(-500)}`);
  return output;
}

export async function fetchRecord(config: SessionConfig, owner: string, repo: string, number: number): Promise<PullRequestRecord> {
  const res = await fetch(`${config.serviceUrl}/pulls/${owner}/${repo}/${number}`, { headers: { authorization: `Bearer ${config.serviceToken}` } });
  if (!res.ok) throw new Error(`cannot read ${owner}/${repo}#${number}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json() as Promise<PullRequestRecord>;
}

async function conventions(api: ToolExecutionApi, dir: string, ctx: Context): Promise<string> {
  for (const name of CONVENTIONS) {
    const read = await api.env!.readTextFile(`${dir}/${name}`, ctx);
    if (read.ok) return `${name}:\n${read.value.slice(0, 12_000)}`;
  }
  return 'No AGENTS.md or CLAUDE.md in the repository root.';
}

// Checks out the pull request's head with its base fetched for diffing, and records the PR for review and judging.
export function checkoutTool(config: SessionConfig) {
  const dir = config.repoDir ?? 'repo';
  return defineTool({
    name: 'checkout_pr',
    description: `Check out a pull request into ./${dir} at its head, with its base fetched, and return its description, CI checks, linked issues and the repo's conventions. Call it first.`,
    parameters: Type.Object({ repository: Type.String({ description: 'owner/repo' }), number: Type.Integer({ minimum: 1 }) }),
    replay: 'safe',
    async execute(args, api, ctx) {
      const [owner, repo] = args.repository.split('/');
      if (!owner || !repo) return { output: [{ type: 'text', text: 'repository must be owner/repo' }], isError: true };
      if (!api.env) return { output: [{ type: 'text', text: 'this session has no workspace' }], isError: true };
      const pr = await fetchRecord(config, owner, repo, args.number);
      const remote = `${config.gitBaseUrl ?? 'https://github.com'}/${owner}/${repo}.git`;
      const cloned = (await api.env.readTextFile(`${dir}/.git/HEAD`, ctx)).ok;
      if (!cloned && pr.baseSha) await git(api, ['init', '-q', dir], config, remote, ctx);
      if (!cloned && pr.baseSha) await git(api, ['-C', dir, 'remote', 'add', 'origin', remote], config, remote, ctx);
      if (!cloned && !pr.baseSha) await git(api, ['clone', '--filter=blob:none', '--no-checkout', remote, dir], config, remote, ctx);
      const refs = pr.baseSha ? ['--no-tags', 'origin', `+${pr.baseSha}:refs/remotes/origin/${pr.baseRef}`, pr.headSha] : ['origin', pr.baseRef, `pull/${pr.number}/head`];
      await git(api, ['-C', dir, 'fetch', '--filter=blob:none', ...refs], config, remote, ctx);
      await git(api, ['-C', dir, 'checkout', '--detach', pr.headSha], config, remote, ctx);
      await api.commit(async (tx) => {
        const d = await tx.doc(PullRequestDoc, api.conversationId);
        d.pr = JSON.parse(JSON.stringify(pr));
        delete d.postedThisCheckout;
      }, ctx);
      const stat = await git(api, ['-C', dir, 'diff', '--stat', `origin/${pr.baseRef}...${pr.headSha}`], config, remote, ctx);
      const hunks = await git(api, ['-C', dir, 'diff', '-U0', `origin/${pr.baseRef}...${pr.headSha}`], config, remote, ctx);
      const text = [
        `${owner}/${repo}#${pr.number} by ${pr.author}: ${pr.title}`,
        `Checked out ${pr.headSha} in ./${dir}; diff against origin/${pr.baseRef}: git -C ${dir} diff origin/${pr.baseRef}...HEAD`,
        `Description:\n${pr.body || '(none)'}`,
        `CI checks: ${pr.checks.map((c) => `${c.name}=${c.conclusion ?? c.status}`).join(', ') || 'none reported'}`,
        ...pr.linkedIssues.map((i) => `Linked issue #${i.number}: ${i.title}\n${i.body}`),
        `Changed files:\n${stat}`,
        await conventions(api, dir, ctx),
        await blastRadius(api, dir, hunks, ctx),
      ].filter(Boolean).join('\n\n');
      return { output: [{ type: 'text', text }] };
    },
  });
}

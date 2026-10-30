import { Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, type Extension } from '@earendil-works/pi-durable';
import { feedback, judge, type CheckResult, type ClassifierRef } from '@quack/judge';
import { artifactsExtension, sealedVersion } from '@quack/sdk';
import { checkoutTool, PullRequestDoc, type SessionConfig } from './checkout.ts';
import { parseReview, reviewEvidence, reviewKind } from './review.ts';
import { reviewerRubric } from './rubric.ts';

const EVENT: Record<string, string> = { approve: 'APPROVE', comment: 'COMMENT', request_changes: 'REQUEST_CHANGES' };
const error = (text: string) => ({ output: [{ type: 'text' as const, text }], isError: true });

// Posts a sealed review version that passed its checks, or reached the revision limit with its failures marked (R3, ART-4).
export function postReviewTool(config: SessionConfig) {
  return defineTool({
    name: 'post_review',
    description: 'Post a checked version of artifacts/review.md to the pull request. Only a version submit_review passed (or released at the revision limit) can be posted.',
    parameters: Type.Object({ version: Type.Integer({ minimum: 1 }) }),
    async execute(args, api, ctx) {
      const sealed = await sealedVersion(api, reviewKind.kind, args.version, ctx);
      if (!sealed) return error(`review v${args.version} is not sealed; call submit_review first`);
      const v = sealed.verdict as { passed?: boolean; limitReached?: boolean; checks?: CheckResult[] } | null;
      if (!v || !(v.passed || v.limitReached)) return error(`review v${args.version} has not passed its checks; revise and call submit_review`);
      const pr = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr;
      if (!pr) return error('no pull request is checked out; call checkout_pr first');
      const marks = v.limitReached ? `\n\n---\n**Not fully checked:** the revision limit was reached. Checks that did not pass:\n${feedback(v.checks ?? []).map((l) => `- ${l}`).join('\n')}\n` : '';
      const body = { owner: pr.owner, repo: pr.repo, number: pr.number, commitId: pr.headSha, event: EVENT[parseReview(sealed.content).verdict ?? ''], body: sealed.content + marks };
      const res = await fetch(`${config.serviceUrl}/reviews`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: `Bearer ${config.serviceToken}` } });
      if (!res.ok) return error(`posting failed: ${res.status} ${(await res.text()).slice(0, 300)}`);
      const posted = (await res.json()) as { url?: string };
      return { output: [{ type: 'text', text: `Posted review v${args.version} as ${body.event}: ${posted.url ?? ''}` }] };
    },
  });
}

export type GithubSessionConfig = SessionConfig & { maxRevisions?: number; classifier?: ClassifierRef };

/** The GitHub extension's session half: checkout, the review artifact, its judge, and the gated post. */
export function githubSessionExtensions(config: GithubSessionConfig): Extension[] {
  return [
    defineExtension({ name: 'github', tools: [checkoutTool(config), postReviewTool(config)] }),
    artifactsExtension([reviewKind]),
    judge({ kind: reviewKind, rubric: reviewerRubric, evidence: reviewEvidence(config.repoDir), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, toolName: 'submit_review' }),
  ];
}

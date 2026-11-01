import { Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, GenerationTask, hook, type Extension } from '@earendil-works/pi-durable';
import { feedback, judge, type CheckResult, type ClassifierRef, type ModelRef } from '@quack/judge';
import { Artifacts, artifactsExtension, attachLink, sealedVersion } from '@quack/sdk';
import { checkoutTool, fetchRecord, PullRequestDoc, type SessionConfig } from './checkout.ts';
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
      const doc = await api.snapshot(PullRequestDoc, api.conversationId, ctx);
      const pr = doc?.pr;
      if (!pr) return error('no pull request is checked out; call checkout_pr first');
      // A push since checkout_pr would post a review of old code against a stale commit; the live head decides.
      const live = await fetchRecord(config, pr.owner, pr.repo, pr.number);
      if (live.headSha !== pr.headSha) return error(`the pull request moved from ${pr.headSha} to ${live.headSha}; call checkout_pr again and review that head`);
      if (doc?.posted?.includes(args.version)) return error(`review v${args.version} was already posted; nothing was sent again`);
      if (doc?.postedThisCheckout) return error(`review v${doc.postedThisCheckout} was already posted for this checkout; a run posts one review, so v${args.version} was not sent`);
      const marks = v.limitReached ? `\n\n---\n**Not fully checked:** the revision limit was reached. Checks that did not pass:\n${feedback(v.checks ?? []).map((l) => `- ${l}`).join('\n')}\n` : '';
      const body = { owner: pr.owner, repo: pr.repo, number: pr.number, commitId: pr.headSha, event: EVENT[parseReview(sealed.content).verdict ?? ''], body: sealed.content + marks };
      const res = await fetch(`${config.serviceUrl}/reviews`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: `Bearer ${config.serviceToken}` } });
      if (!res.ok) return error(`posting failed: ${res.status} ${(await res.text()).slice(0, 300)}`);
      const posted = (await res.json()) as { url?: string };
      await api.commit(async (tx) => { const d = await tx.doc(PullRequestDoc, api.conversationId); d.posted = [...(d.posted ?? []), args.version]; d.postedThisCheckout = args.version; }, ctx);
      if (posted.url) await attachLink(api, reviewKind.kind, args.version, { label: 'Posted', url: posted.url }, ctx);
      return { output: [{ type: 'text', text: `Posted review v${args.version} as ${body.event}${posted.url ? `: ${posted.url}` : ''}` }] };
    },
  });
}

export type GithubSessionConfig = SessionConfig & { maxRevisions?: number; classifier?: ClassifierRef; judgeModel?: ModelRef };

// A run may not finish while its latest review is cleared to go out but has not been posted (R3): delivery is the point.
const postBeforeFinishing = hook(GenerationTask, {
  async onYield(_answer, api, ctx) {
    const last = (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds[reviewKind.kind]?.versions.at(-1);
    const v = last?.verdict as { passed?: boolean; limitReached?: boolean } | null | undefined;
    if (!last || !(v?.passed || v?.limitReached)) return undefined;
    const doc = await api.snapshot(PullRequestDoc, api.conversationId, ctx);
    return doc?.postedThisCheckout || doc?.posted?.includes(last.version) ? undefined : { continue: `Review v${last.version} is cleared to post but has not been posted. Call post_review with version ${last.version}.` };
  },
});

/** The GitHub extension's session half: checkout, the review artifact, its judge, and the gated post. */
export function githubSessionExtensions(config: GithubSessionConfig): Extension[] {
  return [
    defineExtension({ name: 'github', tools: [checkoutTool(config), postReviewTool(config)], hooks: [postBeforeFinishing] }),
    artifactsExtension([reviewKind]),
    judge({ kind: reviewKind, rubric: reviewerRubric, evidence: reviewEvidence(config.repoDir), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, judgeModel: config.judgeModel, toolName: 'submit_review' }),
  ];
}

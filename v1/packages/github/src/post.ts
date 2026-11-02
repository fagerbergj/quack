import type { Context } from '@earendil-works/chord/context';
import { defineExtension, type Extension, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { feedback, judge, type ClassifierRef, type ModelRef, type Verdict } from '@quack/judge';
import { artifactsExtension, attachLink, type SealedArtifact } from '@quack/sdk';
import { checkoutTool, fetchRecord, PullRequestDoc, type SessionConfig } from './checkout.ts';
import { postable } from './inline.ts';
import { replyTool } from './reply.ts';
import { parseReview, reviewEvidence, reviewKind } from './review.ts';
import { reviewerRubric } from './rubric.ts';

const EVENT: Record<string, string> = { approve: 'APPROVE', comment: 'COMMENT', request_changes: 'REQUEST_CHANGES' };

// Delivers a version the judge cleared (passed, or released at the revision limit with its failures marked) once per checkout (R3, ART-4).
function postCleared(config: SessionConfig) {
  return async (sealed: SealedArtifact, v: Verdict, api: ToolExecutionApi, ctx: Context) => {
    const doc = await api.snapshot(PullRequestDoc, api.conversationId, ctx);
    const pr = doc?.pr;
    if (!pr) return { text: 'Nothing was posted: no pull request is checked out; call checkout_pr first.', isError: true };
    const key = String(sealed.version);
    const reviewed = doc.reviewed?.[key];
    if (!reviewed) await api.commit(async (tx) => { const d = await tx.doc(PullRequestDoc, api.conversationId); d.reviewed = { ...d.reviewed, [key]: pr.headSha }; }, ctx);
    if (reviewed && reviewed !== pr.headSha) return { text: `Nothing was posted: v${sealed.version} reviewed ${reviewed}; review the new head and submit a new version.`, isError: true };
    // A push since checkout_pr would post a review of old code against a stale commit; the live head decides.
    const live = await fetchRecord(config, pr.owner, pr.repo, pr.number);
    if (live.headSha !== pr.headSha) return { text: `Nothing was posted: the pull request moved from ${pr.headSha} to ${live.headSha}; check out again with checkout_pr and review that head.`, isError: true };
    if (doc.posted?.includes(sealed.version)) return { text: `Review v${sealed.version} was already posted; nothing was sent again.` };
    if (doc.postedThisCheckout) return { text: `Review v${doc.postedThisCheckout} was already posted for this checkout; a run posts one review, so v${sealed.version} was not sent.` };
    const marks = v.limitReached ? `\n\n---\n**Not fully checked:** the revision limit was reached. Checks that did not pass:\n${feedback(v.checks).map((l) => `- ${l}`).join('\n')}\n` : '';
    const event = EVENT[parseReview(sealed.content).verdict ?? ''];
    const { body: text, comments } = await postable(api, config.repoDir ?? 'repo', pr.baseRef, sealed.content, ctx);
    const body = { owner: pr.owner, repo: pr.repo, number: pr.number, commitId: pr.headSha, event, body: text + marks, ...(comments.length ? { comments } : {}) };
    const res = await fetch(`${config.serviceUrl}/reviews`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: `Bearer ${config.serviceToken}` } });
    if (!res.ok) return { text: `Posting failed: ${res.status} ${(await res.text()).slice(0, 300)}. Call the submit tool again to retry.`, isError: true };
    const posted = (await res.json()) as { url?: string };
    await api.commit(async (tx) => { const d = await tx.doc(PullRequestDoc, api.conversationId); d.posted = [...(d.posted ?? []), sealed.version]; d.postedThisCheckout = sealed.version; }, ctx);
    if (posted.url) await attachLink(api, reviewKind.kind, sealed.version, { label: 'Posted', url: posted.url }, ctx);
    return { text: `Posted as ${event}${posted.url ? `: ${posted.url}` : ''}` };
  };
}

export type GithubSessionConfig = SessionConfig & { maxRevisions?: number; classifier?: ClassifierRef; judgeModel?: ModelRef };

/** The GitHub extension's session half: checkout, the review artifact, its judge, and the gated post. */
export function githubSessionExtensions(config: GithubSessionConfig): Extension[] {
  return [
    defineExtension({ name: 'github', tools: [checkoutTool(config), replyTool(config)] }),
    artifactsExtension([reviewKind]),
    judge({ kind: reviewKind, rubric: reviewerRubric, evidence: reviewEvidence(config.repoDir), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, judgeModel: config.judgeModel, toolName: 'submit_review', onCleared: postCleared(config) }),
  ];
}

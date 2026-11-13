import type { Context } from '@earendil-works/chord/context';
import { defineExtension, type Extension, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { feedback, judged, type Verdict } from '@quack/judge';
import { artifactsExtension, attachLink, gate, sealedVersion, type GateVerdict, type SealedArtifact } from '@quack/sdk';
import { checkoutTool, liveHead, PullRequestDoc, type SessionConfig } from './checkout.ts';
import { postable } from './inline.ts';
import { parseReview, reviewInputs, reviewKind } from './review.ts';
import { reviewDeclaration, reviewerRubric } from './rubric.ts';
import { checkoutTaskTool, deliverTool, prBodyKind } from './task.ts';

const EVENT: Record<string, string> = { approve: 'APPROVE', comment: 'COMMENT', request_changes: 'REQUEST_CHANGES' };

// Only an outage is worth resubmitting; a refusal would fail the same way again.
const postFailed = async (res: Response) => ({
  text: `Posting failed: ${res.status} ${(await res.text()).slice(0, 300)}. ${res.status >= 500 ? 'Call the submit tool again to retry.' : 'GitHub refused it, so a retry would fail the same way.'}`,
  isError: true,
});

// Posts a version once per checkout; it runs after the judge's gate, so with the judge on only a passed or released version gets here.
function post(config: SessionConfig) {
  return async (sealed: SealedArtifact, v: Verdict | null, api: ToolExecutionApi, ctx: Context) => {
    const doc = await api.snapshot(PullRequestDoc, api.conversationId, ctx);
    const pr = doc?.pr;
    if (!pr) return { text: 'Nothing was posted: no pull request is checked out; call checkout_pr first.', isError: true };
    const key = String(sealed.version);
    const reviewed = doc.reviewed?.[key];
    if (!reviewed) await api.commit(async (tx) => { const d = await tx.doc(PullRequestDoc, api.conversationId); d.reviewed = { ...d.reviewed, [key]: pr.headSha }; }, ctx);
    if (reviewed && reviewed !== pr.headSha) return { text: `Nothing was posted: v${sealed.version} reviewed ${reviewed}; review the new head and submit a new version.`, isError: true };
    // A push since checkout_pr would post a review of old code against a stale commit; the live head decides.
    const live = await liveHead(config, pr.owner, pr.repo, pr.number);
    if (live.headSha !== pr.headSha) return { text: `Nothing was posted: the pull request moved from ${pr.headSha} to ${live.headSha}; check out again with checkout_pr and review that head.`, isError: true };
    if (doc.posted?.includes(sealed.version)) return { text: `Review v${sealed.version} was already posted; nothing was sent again.` };
    if (doc.postedThisCheckout) return { text: `Review v${doc.postedThisCheckout} was already posted for this checkout; a run posts one review, so v${sealed.version} was not sent.` };
    const marks = v?.limitReached ? `\n\n---\n**Not fully checked:** the revision limit was reached. Checks that did not pass:\n${feedback(v.checks).map((l) => `- ${l}`).join('\n')}\n` : '';
    const event = EVENT[parseReview(sealed.content).verdict ?? ''];
    const { body: text, comments } = await postable(api, config.repoDir ?? 'repo', pr.baseRef, sealed.content, ctx);
    // The delivery id makes a resubmit after a lost answer or a crash find the posted review instead of posting it again.
    const deliveryId = `${api.conversationId}/v${sealed.version}`;
    const body = { owner: pr.owner, repo: pr.repo, number: pr.number, commitId: pr.headSha, event, body: text + marks, deliveryId, ...(comments.length ? { comments, fallbackBody: sealed.content + marks } : {}) };
    const res = await fetch(`${config.serviceUrl}/reviews`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: `Bearer ${config.serviceToken}` } });
    if (!res.ok) return postFailed(res);
    const posted = (await res.json()) as { url?: string };
    await api.commit(async (tx) => { const d = await tx.doc(PullRequestDoc, api.conversationId); d.posted = [...(d.posted ?? []), sealed.version]; d.postedThisCheckout = sealed.version; }, ctx);
    if (posted.url) await attachLink(api, reviewKind.kind, sealed.version, { label: 'Posted', url: posted.url }, ctx);
    return { text: `Posted as ${event}${posted.url ? `: ${posted.url}` : ''}` };
  };
}

// A checkout that found nothing to review leaves nothing to submit; the refusal ends the run instead of inviting a revision.
async function refuseAfterSkip(api: ToolExecutionApi, ctx: Context) {
  const skipped = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.skipped;
  return skipped && `Nothing to submit: ${skipped}. This run is over.`;
}

export type GithubSessionConfig = SessionConfig & { /** Which job this agent does; reviewer unless set. */ role?: 'reviewer' | 'implementer' };

let unregister: (() => void) | undefined;

// The post gate reads the version back, so it sees the verdict the judge's gate attached before it.
function postGate(config: GithubSessionConfig) {
  const deliver = post(config);
  return async (event: { data: SealedArtifact }, p: { api?: ToolExecutionApi; ctx?: Context }): Promise<GateVerdict | undefined> => {
    if (event.data.kind !== reviewKind.kind) return undefined;
    const sealed = (await sealedVersion(p.api!, reviewKind.kind, event.data.version, p.ctx!))!;
    const out = await deliver(sealed, sealed.verdict as Verdict | null, p.api!, p.ctx!);
    return 'isError' in out && out.isError ? { kind: 'continue', feedback: out.text } : { kind: 'pass', note: out.text };
  };
}

/** The GitHub extension's session half: checkout, the review artifact judged when the agent enables the judge, and its post. */
export function githubSessionExtensions(config: GithubSessionConfig): Extension[] {
  // No judge rubric for the implementer yet (the audit wants a labelled set first): a person's approval gates the push.
  if (config.role === 'implementer') return [defineExtension({ name: 'github', tools: [checkoutTaskTool(config), deliverTool(config)] }), artifactsExtension([prBodyKind])];
  judged(reviewKind.kind, { rubric: reviewerRubric, declaration: reviewDeclaration, inputs: reviewInputs(config.repoDir), refuse: refuseAfterSkip });
  unregister?.();
  unregister = gate('artifact.sealed', postGate(config));
  return [defineExtension({ name: 'github', tools: [checkoutTool(config)] }), artifactsExtension([reviewKind])];
}

// The comments_answered gate of contract.spec v11. It checks naming only: whether a comment counts as addressed is the Contracts app's call.
import type { Approval, SealedArtifact } from '@quack/sdk';
import { lastReviewBlock } from './reason.ts';
import { REPLY_KIND } from './reply.ts';
import type { Change } from './spec.ts';

const idsIn = (reason: string | null | undefined) => lastReviewBlock(reason)?.block.comments.map((c) => c.id) ?? [];
const repliedTo = (v: SealedArtifact) => { try { return (JSON.parse(v.content) as { comment?: unknown }).comment; } catch { return undefined; } };

/** One line per comment the newest rejection sent that no row addresses and no reply answers, and per addressed id no rejection sent.
 * Only rejections decided by the spec's seal count, withdrawn ones never acted, and replies count when sealed by then too. */
export function commentProblems(changes: Change[] | undefined, sealedAt: number, approvals: Record<string, Approval>, kinds: Record<string, { versions: SealedArtifact[] }>): string[] {
  const rejected = Object.values(approvals)
    .filter((a) => a.tool === 'propose_contract' && !a.withdrawn && a.verdict?.decision === 'reject' && a.verdict.at <= sealedAt)
    .sort((a, b) => a.verdict!.at - b.verdict!.at || a.requestedAt - b.requestedAt);
  const sent = new Set(rejected.flatMap((a) => idsIn(a.verdict!.reason)));
  const addressed = new Set((Array.isArray(changes) ? changes : []).flatMap((r) => (Array.isArray(r?.addresses) ? r.addresses.map(String) : [])));
  const replied = new Set((kinds[REPLY_KIND]?.versions ?? []).filter((v) => v.sealedAt <= sealedAt).map(repliedTo));
  const due = [...new Set(idsIn(rejected.at(-1)?.verdict?.reason))].filter((id) => !addressed.has(id) && !replied.has(id));
  return [
    ...due.map((id) => `comment '${id}' from the last Ask for changes is neither addressed nor answered, so name it in the addresses of the changes row that changes its anchored text, or answer it with reply_comment`),
    ...[...addressed].filter((id) => !sent.has(id)).map((id) => `addresses names '${id}', which no Ask for changes sent to this session, so remove it`),
  ];
}

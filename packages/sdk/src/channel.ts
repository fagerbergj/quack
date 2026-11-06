// The lifecycle channel's per-event size cap, and the event data whose size is checked where it is made.
import type { JsonValue } from '@earendil-works/chord';
import type { SealedArtifact } from './artifacts.ts';

/** No channel event may JSON-encode past this, so any one fits a batch under the 1 MiB body limit. */
export const EVENT_MAX_BYTES = 512 * 1024;

const iso = (ms: number) => new Date(ms).toISOString();

/** One event's encoded size, with the widest seq and timestamp it can carry. */
export const eventBytes = (id: string, type: string, data: unknown): number =>
  Buffer.byteLength(JSON.stringify({ seq: Number.MAX_SAFE_INTEGER, id, at: '+275760-09-13T00:00:00.000Z', type, data }));

/** artifact.sealed's data for one sealed version. */
export function artifactSealedData(kind: string, v: SealedArtifact) {
  return {
    kind, version: v.version, hash: v.hash, content: v.content, sealedAt: iso(v.sealedAt), verdict: v.verdict ?? null, links: v.links ?? [],
    ...(v.summary !== undefined && { summary: v.summary }), ...(v.outcome !== undefined && { outcome: v.outcome }), ...(v.basis !== undefined && { basis: v.basis }), ...(v.carriedFrom && { carriedFrom: v.carriedFrom }),
  };
}

/** ask.opened's data for one gated tool call. */
export const approvalOpenedData = (id: string, tool: string, args: JsonValue, requestedAt: number) =>
  ({ id, type: 'approval', createdAt: iso(requestedAt), toolCall: { tool, args } });

/** Throws when a sealed version's event would pass the cap, saying what the agent can change. */
export function assertSealFits(kind: string, v: SealedArtifact): void {
  const size = eventBytes(`artifact:${kind}:${v.version}`, 'artifact.sealed', artifactSealedData(kind, v));
  if (size > EVENT_MAX_BYTES) throw new Error(`${kind} v${v.version} is ${size} bytes as a lifecycle event, over the ${EVENT_MAX_BYTES}-byte cap; make the content shorter`);
}

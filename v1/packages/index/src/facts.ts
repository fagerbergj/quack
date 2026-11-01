import type { Context } from '@earendil-works/chord/context';
import { defineDoc, UsageDoc, type Conversation, type Harness, type JsonObject, type UsageState } from '@earendil-works/pi-durable';
import { Approvals, Artifacts, type Approval, type SealedArtifact } from '@quack/sdk';

/** One thing the index records about a session. `rev` only grows, so an older copy never overwrites a newer one. */
export type Fact = { id: string; kind: 'session' | 'artifact' | 'approval' | 'cost' | 'status'; rev: number; at: number; data: JsonObject };
export type SessionMeta = { key: string; agent: string; version: number | null };

type OutboxDoc = { acked: Record<string, number>; status: { state: 'idle' | 'running'; seq: number; at: number } };

/** What the index has acknowledged, and the run status; the unsent facts are whatever the log holds beyond `acked`. */
export const Outbox = defineDoc<OutboxDoc>({
  kind: 'quack.index-outbox', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ acked: {}, status: { state: 'idle', seq: 0, at: 0 } }), checkpointWhen: () => false,
});

const verdictOf = (v: { verdict: unknown }) => (v.verdict ? { passed: (v.verdict as { passed?: boolean }).passed === true } : null);

function artifactFacts(artifacts: { kinds: Record<string, { versions: SealedArtifact[] }> } | undefined): Fact[] {
  return Object.entries(artifacts?.kinds ?? {}).flatMap(([kind, { versions }]) =>
    versions.map((v): Fact => ({ id: `artifact:${kind}:${v.version}`, kind: 'artifact', rev: v.verdict ? 2 : 1, at: v.sealedAt, data: { kind, version: v.version, hash: v.hash, verdict: verdictOf(v) } })));
}

function approvalFacts(approvals: { items: Record<string, Approval> } | undefined): Fact[] {
  return Object.entries(approvals?.items ?? {}).map(([id, a]): Fact => {
    const v = a.verdict;
    return { id: `approval:${id}`, kind: 'approval', rev: v ? 2 : 1, at: v?.at ?? a.requestedAt, data: { tool: a.tool, state: v ? 'resolved' : 'pending', decision: v?.decision ?? null, by: v?.by ?? null } };
  });
}

function costFact(usage: Readonly<UsageState> | undefined, now: number): Fact[] {
  const total = [...Object.values(usage?.models ?? {}), ...Object.values(usage?.tools ?? {})].reduce((sum, u) => sum + ((u as { cost?: { total?: number } }).cost?.total ?? 0), 0);
  // Micro-dollars as the revision: it moves only when the spend does, so an idle session sends nothing.
  return total > 0 ? [{ id: 'cost', kind: 'cost', rev: Math.max(1, Math.round(total * 1e6)), at: now, data: { total } }] : [];
}

// Derived from the log's current documents, so a replay of the log yields the same facts as live publishing.
export async function collectFacts(harness: Harness, root: Conversation, meta: SessionMeta, now: number, ctx: Context): Promise<Fact[]> {
  const [artifacts, approvals, usage, outbox] = await Promise.all([
    harness.snapshot(Artifacts, root.id, ctx), harness.snapshot(Approvals, root.id, ctx), harness.snapshot(UsageDoc, root.id, ctx), harness.snapshot(Outbox, root.id, ctx),
  ]);
  const status = outbox?.status;
  return [
    { id: 'session', kind: 'session', rev: 1, at: 0, data: { agent: meta.agent, version: meta.version } },
    ...artifactFacts(artifacts), ...approvalFacts(approvals), ...costFact(usage, now),
    ...(status?.seq ? [{ id: 'status', kind: 'status' as const, rev: status.seq, at: status.at, data: { state: status.state } }] : []),
  ];
}

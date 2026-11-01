import type { Context } from '@earendil-works/chord/context';
import { defineDoc, UsageDoc, type Conversation, type Harness, type JsonObject } from '@earendil-works/pi-durable';
import { Approvals, Artifacts } from '@quack/sdk';

/** One thing the index records about a session. `rev` only grows, so an older copy never overwrites a newer one. */
export type Fact = { id: string; kind: 'session' | 'artifact' | 'approval' | 'cost' | 'status'; rev: number; at: number; data: JsonObject };
export type SessionMeta = { key: string; agent: string; version: number | null };

type OutboxDoc = { acked: Record<string, number>; status: { state: 'idle' | 'running'; seq: number; at: number } };

/** What the index has acknowledged, and the run status; the unsent facts are whatever the log holds beyond `acked`. */
export const Outbox = defineDoc<OutboxDoc>({
  kind: 'quack.index-outbox', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ acked: {}, status: { state: 'idle', seq: 0, at: 0 } }), checkpointWhen: () => false,
});

// Derived from the log's current documents, so a replay of the log yields the same facts as live publishing.
export async function collectFacts(harness: Harness, root: Conversation, meta: SessionMeta, now: number, ctx: Context): Promise<Fact[]> {
  const [artifacts, approvals, usage, outbox] = await Promise.all([
    harness.snapshot(Artifacts, root.id, ctx), harness.snapshot(Approvals, root.id, ctx), harness.snapshot(UsageDoc, root.id, ctx), harness.snapshot(Outbox, root.id, ctx),
  ]);
  const facts: Fact[] = [{ id: 'session', kind: 'session', rev: 1, at: 0, data: { agent: meta.agent, version: meta.version } }];
  for (const [kind, { versions }] of Object.entries(artifacts?.kinds ?? {})) {
    for (const v of versions) {
      const verdict = v.verdict ? { passed: (v.verdict as { passed?: boolean }).passed === true } : null;
      facts.push({ id: `artifact:${kind}:${v.version}`, kind: 'artifact', rev: verdict ? 2 : 1, at: v.sealedAt, data: { kind, version: v.version, hash: v.hash, verdict } });
    }
  }
  for (const [id, a] of Object.entries(approvals?.items ?? {})) {
    const v = a.verdict;
    facts.push({ id: `approval:${id}`, kind: 'approval', rev: v ? 2 : 1, at: v?.at ?? a.requestedAt, data: { tool: a.tool, state: v ? 'resolved' : 'pending', decision: v?.decision ?? null, by: v?.by ?? null } });
  }
  const total = [...Object.values(usage?.models ?? {}), ...Object.values(usage?.tools ?? {})].reduce((sum, u) => sum + ((u as { cost?: { total?: number } }).cost?.total ?? 0), 0);
  // Micro-dollars as the revision: it moves only when the spend does, so an idle session sends nothing.
  if (total > 0) facts.push({ id: 'cost', kind: 'cost', rev: Math.max(1, Math.round(total * 1e6)), at: now, data: { total } });
  const status = outbox?.status;
  if (status?.seq) facts.push({ id: 'status', kind: 'status', rev: status.seq, at: status.at, data: { state: status.state } });
  return facts;
}

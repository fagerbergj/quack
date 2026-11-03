import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type Conversation, type Harness, type JsonObject, type UsageState } from '@earendil-works/pi-durable';
import { Approvals, Artifacts, Failures, Questions, type Approval, type FailureRow, type Question, type SealedArtifact } from '@quack/sdk';

/** One thing the index records about a session. `rev` only grows, so an older copy never overwrites a newer one. */
export type Fact = { id: string; kind: 'session' | 'artifact' | 'approval' | 'question' | 'failure' | 'cost' | 'status'; rev: number; at: number; data: JsonObject };
export type SessionMeta = { key: string; agent: string; version: number | null };

export type Spend = { input: number; output: number; cacheRead: number; cacheWrite: number; cost: number; /** When the last one was made; pi-durable's totals carry no time, so a fact dated by them would differ between live and replay. */ at: number };
/** `direct` is the spend of completions made outside the session's generation task (workflow steps), which pi-durable's usage totals never see. */
type OutboxDoc = { acked: Record<string, number>; status: { state: 'idle' | 'running'; seq: number; at: number }; direct?: Spend };

/** What the index has acknowledged, and the run status; the unsent facts are whatever the log holds beyond `acked`. */
export const Outbox = defineDoc<OutboxDoc>({
  kind: 'quack.index-outbox', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ acked: {}, status: { state: 'idle', seq: 0, at: 0 } }), checkpointWhen: () => false,
});

// limitReached marks a version released with its failures at the revision limit, which is final even though it did not pass.
const verdictOf = (v: { verdict: unknown }) => {
  const d = v.verdict as { passed?: boolean; limitReached?: boolean } | null;
  return d ? { passed: d.passed === true, ...(d.limitReached === true && { limitReached: true }) } : null;
};

const linksOf = (v: SealedArtifact) => v.links ?? [];

/** The rev always carries the outcome's +2, even when absent, so facts emitted before `outcome` was stored (which had it) are never outranked by the re-emit. */
export function artifactFacts(artifacts: { kinds: Record<string, { versions: SealedArtifact[] }> } | undefined): Fact[] {
  return Object.entries(artifacts?.kinds ?? {}).flatMap(([kind, { versions }]) =>
    versions.map((v): Fact => ({ id: `artifact:${kind}:${v.version}`, kind: 'artifact', rev: (v.verdict ? 2 : 1) + linksOf(v).length + 2, at: v.sealedAt, data: { kind, version: v.version, hash: v.hash, verdict: verdictOf(v), links: linksOf(v), outcome: v.outcome ?? null, summary: v.summary ?? null } })));
}

function approvalFacts(approvals: { items: Record<string, Approval> } | undefined): Fact[] {
  return Object.entries(approvals?.items ?? {}).map(([id, a]): Fact => {
    const v = a.verdict;
    return { id: `approval:${id}`, kind: 'approval', rev: v ? 2 : 1, at: v?.at ?? a.requestedAt, data: { tool: a.tool, state: v ? 'resolved' : 'pending', decision: v?.decision ?? null, by: v?.by ?? null, args: v ? null : a.args } };
  });
}

// An answer or a close is the second revision of a question; ids carry the asking task, so a later run's question is a new row.
function questionFacts(questions: { items: Record<string, Question> } | undefined): Fact[] {
  return Object.entries(questions?.items ?? {}).map(([id, q]): Fact => {
    const state = q.answer ? 'answered' : q.closed ? 'closed' : 'pending';
    return {
      id: `question:${id}`, kind: 'question', rev: state === 'pending' ? 1 : 2, at: q.answer?.at ?? q.askedAt,
      data: { question: q.question, options: q.options, evidence: q.evidence, askedAt: q.askedAt, state, choice: q.answer?.choice ?? null, by: q.answer?.by ?? null },
    };
  });
}

// Every step of the current run is a row, and the run's task id orders the revisions, so a later run supersedes an earlier run's failure.
function failureFacts(failures: { items: Record<string, FailureRow> } | undefined): Fact[] {
  return Object.entries(failures?.items ?? {}).map(([step, r]): Fact => {
    const failed = r.error !== null;
    return { id: `failure:${step}`, kind: 'failure', rev: r.run * 2 + (failed ? 2 : 1), at: r.at, data: { step, error: r.error ?? '', state: failed ? 'failed' : 'cleared' } };
  });
}

/** Per-million price of a `provider/modelId` key as the deployment configures it; omitted parts are free. */
export type PriceOf = (modelKey: string) => { input?: number; output?: number; cacheRead?: number; cacheWrite?: number } | undefined;
const none: PriceOf = () => undefined;

// Cost is tokens at today's price, not the per-call cost stored when each call ran, so a price set or changed later reaches old calls.
// Tool and direct spend carry no model key and stay as stored. A lower price does not republish: the index keeps the higher rev.
function costFact(usage: Readonly<UsageState>, direct: Spend | undefined, priceOf: PriceOf): Fact[] {
  const t: Spend = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0, ...direct };
  const add = (u: Readonly<UsageState['tools'][string]>, p?: ReturnType<PriceOf>) => {
    t.input += u.input; t.output += u.output; t.cacheRead += u.cacheRead; t.cacheWrite += u.cacheWrite;
    t.cost += p ? (u.input * (p.input ?? 0) + u.output * (p.output ?? 0) + u.cacheRead * (p.cacheRead ?? 0) + u.cacheWrite * (p.cacheWrite ?? 0)) / 1e6 : u.cost?.total ?? 0;
  };
  for (const [key, u] of Object.entries(usage.models)) add(u, priceOf(key));
  for (const u of Object.values(usage.tools)) add(u);
  // Tokens plus micro-dollars as the revision: both only grow, so it moves only when the spend does, and a free model still publishes its tokens.
  const rev = t.input + t.output + t.cacheRead + t.cacheWrite + Math.round(t.cost * 1e6);
  return rev > 0 ? [{ id: 'cost', kind: 'cost', rev, at: t.at, data: { total: t.cost, input: t.input, output: t.output, cacheRead: t.cacheRead, cacheWrite: t.cacheWrite } }] : [];
}

// harness.usage totals every conversation, so decision forks and subagents count. Derived from the log's current documents, so a replay of the log yields the same facts as live publishing.
export async function collectFacts(harness: Harness, root: Conversation, meta: SessionMeta, now: number, ctx: Context, priceOf: PriceOf = none): Promise<Fact[]> {
  const [artifacts, approvals, questions, failures, outbox] = await Promise.all([
    harness.snapshot(Artifacts, root.id, ctx), harness.snapshot(Approvals, root.id, ctx), harness.snapshot(Questions, root.id, ctx), harness.snapshot(Failures, root.id, ctx), harness.snapshot(Outbox, root.id, ctx),
  ]);
  const status = outbox?.status;
  return [
    { id: 'session', kind: 'session', rev: 1, at: 0, data: { agent: meta.agent, version: meta.version } },
    ...artifactFacts(artifacts), ...approvalFacts(approvals), ...questionFacts(questions), ...failureFacts(failures), ...costFact(await harness.usage(ctx), outbox?.direct, priceOf),
    ...(status?.seq ? [{ id: 'status', kind: 'status' as const, rev: status.seq, at: status.at, data: { state: status.state } }] : []),
  ];
}

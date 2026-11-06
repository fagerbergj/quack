// A session's lifecycle event stream for the distributor's catalog, derived from its log and kept in it: the log is the outbox.
import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type Conversation, type Harness, type JsonObject, type UsageState } from '@earendil-works/pi-durable';
import { approvalOpenedData, Approvals, artifactSealedData, Artifacts, eventBytes, EVENT_MAX_BYTES, Failures, failedRows, Questions, type Approval, type Principal, type Question, type SealedArtifact } from '@quack/sdk';

/** One channel event as the lifecycle channel carries it. */
export type WireEvent = { seq: number; id: string; at: string; type: string; data: JsonObject };
type Logical = Omit<WireEvent, 'seq'>;

/** One run as the session recorded it when it started and ended. */
export type RunRecord = { n: number; startedAt: number; endedAt: number | null; failedSteps: Array<{ step: string; error: string }>; lastMessage: string | null; cancelled: string[] };
export type Spend = { input: number; output: number; cacheRead: number; cacheWrite: number; cost: number; at: number };

// Every revision of every event, append-only, so a replay from any watermark is contiguous; `signatures` finds what changed.
type StreamDoc = { events: WireEvent[]; signatures: Record<string, string>; acked: number; runs: RunRecord[]; direct?: Spend };

export const LifecycleStream = defineDoc<StreamDoc>({
  kind: 'quack.lifecycle-stream', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ events: [], signatures: {}, acked: 0, runs: [] }), checkpointWhen: () => false,
});

// The index's outbox, read once for the workflow spend it held, so a session that predates the stream keeps it.
type LegacyOutbox = { acked: Record<string, number>; status: { state: 'idle' | 'running'; seq: number; at: number }; direct?: Spend };
export const LegacyIndexOutbox = defineDoc<LegacyOutbox>({
  kind: 'quack.index-outbox', version: 1, scope: 'conversation', history: 'latest', fork: 'initial',
  initial: () => ({ acked: {}, status: { state: 'idle', seq: 0, at: 0 } }), checkpointWhen: () => false,
});

/** What the session knows about itself beyond its documents; read the same way by the session and by the distributor's backfill. */
export type SessionFacts = { agent: string; version: number | null; createdAt: number; title?: string; lastActivity?: number };

/** Per-million price of a `provider/modelId` key as the deployment configures it; omitted parts are free. */
export type PriceOf = (modelKey: string) => { input?: number; output?: number; cacheRead?: number; cacheWrite?: number } | undefined;

export const CURRENCY = 'USD';
const iso = (ms: number) => new Date(ms).toISOString();
const ev = (id: string, type: string, at: number, data: Record<string, unknown>): Logical => ({ id, type, at: iso(at), data: JSON.parse(JSON.stringify(data)) as JsonObject });

const principal = (by: Principal | string): JsonObject => {
  if (typeof by !== 'string') return by.kind === 'person' ? { kind: 'person', id: by.name } : { kind: 'tailnet' };
  return by === 'tailnet' ? { kind: 'tailnet' } : { kind: 'person', id: by };
};

function runEvents(runs: RunRecord[], facts: SessionFacts, failures: Array<{ step: string; error: string }>): Logical[] {
  // A session from before the stream has no runs; its last activity stands in as one ended run.
  if (!runs.length) {
    return facts.lastActivity && facts.lastActivity > facts.createdAt
      ? [ev('run:legacy:ended', 'run.ended', facts.lastActivity, { run: 1, outcome: failures.length ? 'failed' : 'succeeded', failedSteps: failures })] : [];
  }
  return runs.flatMap((r) => [
    ev(`run:${r.n}:started`, 'run.started', r.startedAt, { run: r.n }),
    ...(r.endedAt === null ? [] : [
      // shortcut: an aborted run reads as failed or succeeded; report aborted once the harness exposes how a run ended.
      ev(`run:${r.n}:ended`, 'run.ended', r.endedAt, { run: r.n, outcome: r.failedSteps.length ? 'failed' : 'succeeded', failedSteps: r.failedSteps, ...(r.lastMessage && { lastMessage: r.lastMessage }) }),
      ev(`state:run:${r.n}:idle`, 'state.changed', r.endedAt, { state: 'idle' }),
    ]),
  ]);
}

function approvalEvents(id: string, a: Approval, cancelledBy: Map<string, number>): Logical[] {
  const opened = ev(`ask:open:${id}`, 'ask.opened', a.requestedAt, approvalOpenedData(id, a.tool, a.args, a.requestedAt));
  const v = a.verdict;
  if (v) return [opened, ev(`ask:resolve:${id}`, 'ask.resolved', v.at, { id, choice: v.decision, ...(v.reason !== null && { reason: v.reason }), by: principal(v.by) })];
  const run = cancelledBy.get(id);
  return run === undefined ? [opened] : [opened, ev(`ask:cancel:${id}`, 'ask.cancelled', a.requestedAt, { id, run })];
}

function questionEvents(id: string, q: Question, lastRun: number): Logical[] {
  const prompt = {
    question: q.question, options: q.options, freeText: q.freeText ?? false, evidence: q.evidence, header: q.header ?? null, title: q.title ?? null, detail: q.detail ?? null,
    suggested: q.suggested ?? null, ...(q.optionDescriptions && { optionDescriptions: q.optionDescriptions }),
  };
  const opened = ev(`ask:open:${id}`, 'ask.opened', q.askedAt, { id, type: 'question', createdAt: iso(q.askedAt), prompt });
  const a = q.answer;
  if (a) return [opened, ev(`ask:resolve:${id}`, 'ask.resolved', a.at, { id, choice: a.choice, ...(a.text && { text: a.text }), ...(a.note && { reason: a.note }), by: principal(a.by) })];
  return q.closed ? [opened, ev(`ask:cancel:${id}`, 'ask.cancelled', q.askedAt, { id, run: Math.max(1, lastRun) })] : [opened];
}

const artifactEvent = (kind: string, v: SealedArtifact): Logical => ev(`artifact:${kind}:${v.version}`, 'artifact.sealed', v.sealedAt, artifactSealedData(kind, v));

const MARKER = ' … [truncated]';
const cut = (text: string, chars: number) => (text.length <= chars ? text : `${text.slice(0, Math.max(0, chars))}${MARKER}`);
const sizeOf = (e: Logical) => eventBytes(e.id, e.type, e.data);

// The free text an over-cap event may lose, longest first, cut until the event fits; the gate and the seal refuse the rest.
function fitted(e: Logical): Logical | undefined {
  const d = e.data as Record<string, unknown>;
  const texts: Array<[get: () => string, set: (t: string) => void]> = [];
  if (e.type === 'run.ended') {
    if (typeof d.lastMessage === 'string') texts.push([() => d.lastMessage as string, (t) => (d.lastMessage = t)]);
    for (const step of d.failedSteps as Array<{ error: string }>) texts.push([() => step.error, (t) => (step.error = t)]);
  }
  const prompt = d.prompt as Record<string, unknown> | undefined;
  for (const k of ['evidence', 'detail'] as const) if (typeof prompt?.[k] === 'string') texts.push([() => prompt[k] as string, (t) => (prompt[k] = t)]);
  for (let over = sizeOf(e) - EVENT_MAX_BYTES; over > 0 && texts.some(([get]) => !get().endsWith(MARKER) && get().length > 0); over = sizeOf(e) - EVENT_MAX_BYTES) {
    const [get, set] = texts.filter(([g]) => !g().endsWith(MARKER)).sort(([a], [b]) => b().length - a().length)[0]!;
    set(cut(get(), get().length - over - Buffer.byteLength(MARKER)));
  }
  return sizeOf(e) <= EVENT_MAX_BYTES ? e : undefined;
}

// Cost is tokens at today's price, so a price set or changed later reaches old calls; tool and direct spend stay as stored.
export function spendOf(usage: Readonly<UsageState>, direct: Spend | undefined, priceOf: PriceOf): Spend {
  const t: Spend = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0, ...direct };
  const add = (u: Readonly<UsageState['tools'][string]>, p?: ReturnType<PriceOf>) => {
    t.input += u.input; t.output += u.output; t.cacheRead += u.cacheRead; t.cacheWrite += u.cacheWrite;
    t.cost += p ? (u.input * (p.input ?? 0) + u.output * (p.output ?? 0) + u.cacheRead * (p.cacheRead ?? 0) + u.cacheWrite * (p.cacheWrite ?? 0)) / 1e6 : u.cost?.total ?? 0;
  };
  for (const [key, u] of Object.entries(usage.models)) add(u, priceOf(key));
  for (const u of Object.values(usage.tools)) add(u);
  return t;
}

// The micros never shrink: a price cut must not read as spend going backwards, which the catalog refuses.
const spendEvent = (s: Spend, createdAt: number, floor: bigint): Logical => {
  const micros = BigInt(Math.round(s.cost * 1e6));
  return ev('spend', 'spend.recorded', s.at || createdAt, {
    cost: { currencyCode: CURRENCY, amountMicros: String(micros > floor ? micros : floor) },
    tokens: { input: s.input, output: s.output, cacheRead: s.cacheRead, cacheWrite: s.cacheWrite },
  });
};

/** The session's events at their latest revision, in a fixed order: a pure function of its documents and facts. */
export function currentEvents(rootId: string, facts: SessionFacts, docs: {
  approvals?: Readonly<{ items: Record<string, Approval> }> | null; questions?: Readonly<{ items: Record<string, Question> }> | null;
  artifacts?: Readonly<{ kinds: Record<string, { versions: SealedArtifact[] }> }> | null; failures?: Parameters<typeof failedRows>[0]; runs: RunRecord[]; spend: Spend; spentMicros: bigint;
}): Logical[] {
  const cancelledBy = new Map(docs.runs.flatMap((r) => r.cancelled.map((id) => [id, r.n] as const)));
  const lastRun = docs.runs.at(-1)?.n ?? 1;
  return [
    ev(`session:${rootId}`, 'session.created', facts.createdAt, { agent: facts.agent, version: facts.version, createdAt: iso(facts.createdAt) }),
    ...(facts.title ? [ev('title', 'title.set', facts.createdAt, { title: facts.title.slice(0, 120) })] : []),
    ...runEvents(docs.runs, facts, failedRows(docs.failures)),
    ...Object.entries(docs.approvals?.items ?? {}).flatMap(([id, a]) => approvalEvents(id, a, cancelledBy)),
    ...Object.entries(docs.questions?.items ?? {}).flatMap(([id, q]) => questionEvents(id, q, lastRun)),
    ...Object.entries(docs.artifacts?.kinds ?? {}).flatMap(([kind, { versions }]) => versions.map((v) => artifactEvent(kind, v))),
    ...((docs.spend.input + docs.spend.output + docs.spend.cacheRead + docs.spend.cacheWrite > 0 || docs.spend.cost > 0) ? [spendEvent(docs.spend, facts.createdAt, docs.spentMicros)] : []),
  ];
}

const signature = (e: Logical) => JSON.stringify([e.type, e.at, e.data]);

/** The revisions to append: every event that is new or changed since the stream last recorded it, numbered on from its end. */
export function revisions(stream: Readonly<StreamDoc>, current: Logical[], log = (m: string) => console.error(m)): WireEvent[] {
  let seq = stream.events.length;
  return current.flatMap((raw) => {
    const e = fitted(raw);
    if (!e) return void log(`lifecycle: event ${raw.id} is over the ${EVENT_MAX_BYTES}-byte cap and was not recorded`), [];
    return stream.signatures[e.id] === signature(e) ? [] : [{ seq: ++seq, ...e }];
  });
}

/** Records the revisions in the stream document; the caller commits it. */
export function append(doc: StreamDoc, fresh: WireEvent[]): void {
  for (const e of fresh) {
    doc.events.push(e);
    doc.signatures[e.id] = signature(e);
  }
}

/** The highest spend the stream has already reported, so a later revision never reports less. */
export function reportedMicros(stream: Readonly<StreamDoc>): bigint {
  const last = [...stream.events].reverse().find((e) => e.type === 'spend.recorded');
  return last ? BigInt(String((last.data.cost as { amountMicros: string }).amountMicros)) : 0n;
}

/** Reads every input of the derivation from the log, for the sender and for the distributor's backfill alike. */
export async function readStream(harness: Harness, root: Conversation, facts: SessionFacts, priceOf: PriceOf, ctx: Context) {
  const id = root.id;
  const stream = await harness.snapshot(LifecycleStream, id, ctx);
  const legacy = await harness.snapshot(LegacyIndexOutbox, id, ctx);
  const [approvals, questions, artifacts, failures] = await Promise.all([harness.snapshot(Approvals, id, ctx), harness.snapshot(Questions, id, ctx), harness.snapshot(Artifacts, id, ctx), harness.snapshot(Failures, id, ctx)]);
  // A document never written reads as its initial value, so a stream is recorded once it holds its session.created.
  const recorded = (stream?.events.length ?? 0) > 0;
  const legacyDirect = !recorded && !stream?.direct && legacy?.direct ? legacy.direct : undefined;
  const doc: StreamDoc = recorded ? stream! : { events: [], signatures: {}, acked: 0, runs: stream?.runs ?? [], ...((stream?.direct ?? legacyDirect) && { direct: stream?.direct ?? legacyDirect }) };
  const spend = spendOf(await harness.usage(ctx), doc.direct, priceOf);
  // Last activity stands in for runs only in the first derivation; once the stream exists its runs are recorded.
  const current = currentEvents(String(root.id), recorded ? { ...facts, lastActivity: undefined } : facts, { approvals, questions, artifacts, failures, runs: doc.runs, spend, spentMicros: reportedMicros(doc) });
  return { doc, recorded, fresh: revisions(doc, current), legacyDirect };
}


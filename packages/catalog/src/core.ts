import { randomUUID } from 'node:crypto';
import type { ActionMethod, Journal, JournalLine } from './journal.ts';
import { parseQuery, QueryError, type ListName } from './query.ts';
import type {
  ActionWrite, AskPrompt, AskType, EventWrite, FailedStep, Json, Money, Principal, QueryAst, RunOutcome, SessionCatalogStore, SessionOrder, TokenUsage,
} from './port.ts';

/** One core lifecycle channel event in domain values: epoch ms, bigint micros. */
export type CoreEvent = { seq: number; id: string; at: number } & (
  | { type: 'session.created'; data: { agent: string; version?: number | null; createdAt: number } }
  | { type: 'run.started'; data: { run: number } }
  | { type: 'run.ended'; data: { run: number; outcome: RunOutcome; failedSteps: FailedStep[]; lastMessage?: string } }
  | { type: 'ask.opened'; data: { id: string; type: AskType; createdAt: number; toolCall?: { tool: string; args: Json }; prompt?: AskPrompt } }
  | { type: 'ask.resolved'; data: { id: string; choice: string; text?: string; reason?: string; by: Principal } }
  | { type: 'ask.cancelled'; data: { id: string; run: number } }
  | { type: 'artifact.sealed'; data: ArtifactSeal }
  | { type: 'spend.recorded'; data: { cost: Money; tokens: TokenUsage } }
  | { type: 'title.set'; data: { title: string } }
  | { type: 'state.changed'; data: { state: 'running' | 'idle' | 'stopped' | 'archived' } }
  | { type: 'artifact.delivered'; data: { kind: string; version: number; where: string; link: string; at: number } }
);
/** A namespaced <extension>.<event>: the catalog stores nothing of it and only moves its watermark past it. */
export type ExtensionEvent = { seq: number; id: string; at: number; type: string; data: { [field: string]: Json }; extension: true };
export type CatalogEvent = CoreEvent | ExtensionEvent;
type ArtifactSeal = { kind: string; version: number; hash: string; content: string; sealedAt?: number; summary?: string };

/** A channel batch the core refuses whole: the channel's 400. */
export class BatchError extends Error {}

/** A list request as the REST surface takes it; the core parses, validates and caps it. */
export type ListRequest = { q?: string | null; orderBy?: string | null; pageSize?: number; pageToken?: string };

export const DEFAULT_PAGE_SIZE = 100;
export const MAX_PAGE_SIZE = 1000;

// The orders each list allows, by the REST orderBy value, with the list's default first.
const SESSION_ORDERS = { lastActivity: 'newest', createdAt: 'newest', oldestAskAt: 'oldest' } as const;

function orderOf<T extends Record<string, 'newest' | 'oldest'>>(allowed: T, requested: string | null | undefined) {
  const field = (requested || Object.keys(allowed)[0]) as keyof T & string;
  if (!Object.hasOwn(allowed, field)) throw new QueryError(`orderBy must be one of ${Object.keys(allowed).join(', ')}`);
  return { field, direction: allowed[field]! };
}

function pageOf(req: ListRequest) {
  const size = req.pageSize ?? DEFAULT_PAGE_SIZE;
  if (!Number.isInteger(size) || size < 1 || size > MAX_PAGE_SIZE) throw new QueryError(`pageSize must be an integer from 1 to ${MAX_PAGE_SIZE}`);
  return { pageSize: size, ...(req.pageToken && { cursor: req.pageToken }) };
}

// A per-session list is the cross-session one with its session folded into where.
function scoped<F extends string>(list: ListName, q: string | null | undefined, session?: string): QueryAst<F> {
  const where = parseQuery(list, q) as QueryAst<F>;
  if (session === undefined) return where;
  const own = { kind: 'match', field: 'session', value: session } as QueryAst<F>;
  return where.kind === 'all' ? own : { kind: 'and', terms: [own, where] };
}

/** The catalog core: maps channel events and action writes onto the port, journals every action, and rebuilds. */
export function createCatalog(opts: { store: SessionCatalogStore; journal: Journal; now?: () => number; newId?: () => string; log?: (message: string) => void }) {
  const { store, journal } = opts;
  const now = opts.now ?? Date.now;
  const newId = opts.newId ?? randomUUID;
  const log = opts.log ?? ((m: string) => console.error(m));

  let ready: Promise<void> | undefined;
  // Memoized once it succeeds, retried while the database is down, so the distributor can start before it.
  const ensure = () => (ready ??= store.migrate().catch((err) => { ready = undefined; throw err; }));

  // Writes for one session run one at a time, so a journaled watermark is the one its write applied at.
  const chains = new Map<string, Promise<unknown>>();
  function serial<T>(key: string, fn: () => Promise<T>): Promise<T> {
    const next = (chains.get(key) ?? Promise.resolve()).then(fn, fn);
    const tail = next.then(() => {}, () => {});
    chains.set(key, tail);
    void tail.then(() => chains.get(key) === tail && chains.delete(key));
    return next;
  }

  async function applyEvent(key: string, e: CatalogEvent): Promise<void> {
    const w: EventWrite = { kind: 'event', id: e.id, seq: e.seq };
    if ('extension' in e) return store.advanceWatermark(w, { key });
    switch (e.type) {
      case 'session.created': return store.createSession(w, { key, agent: e.data.agent, createdAt: e.data.createdAt });
      case 'run.started': return void (await store.transitionSession(w, { key, to: 'running' }));
      // stopped and archived are the echo of the action write that set them, which is authoritative.
      case 'state.changed': return e.data.state === 'stopped' || e.data.state === 'archived' ? store.advanceWatermark(w, { key }) : void (await store.transitionSession(w, { key, to: e.data.state }));
      case 'artifact.delivered': return store.recordArtifactDelivery(w, { key, ...e.data });
      case 'run.ended': return store.recordRunOutcome(w, { key, outcome: e.data.outcome, endedAt: e.at, failedSteps: e.data.failedSteps, ...(e.data.lastMessage !== undefined && { lastMessage: e.data.lastMessage }) });
      case 'ask.opened': return store.openAsk(w, { key, ...e.data });
      case 'ask.resolved': return void (await store.resolveAsk(w, { key, ...e.data, resolvedAt: e.at }));
      case 'ask.cancelled': return store.cancelAsk(w, { key, id: e.data.id });
      case 'title.set': return store.setTitle(w, { key, title: e.data.title });
      case 'artifact.sealed': {
        const { sealedAt, ...seal } = e.data;
        return store.sealArtifact(w, { key, ...seal, sealedAt: sealedAt ?? e.at });
      }
      case 'spend.recorded': {
        const out = await store.recordSpend(w, { key, ...e.data });
        // A session bug: spend in another currency, or going backwards. Refused with the watermark held.
        if (out.refused) log(`catalog: session ${key} spend at seq ${e.seq} refused (${out.refused})`);
      }
    }
  }

  function replayAction(line: Extract<JournalLine, { method: ActionMethod }>): Promise<unknown> {
    const w: ActionWrite = { kind: 'action', id: line.id };
    const args = line.args as never;
    switch (line.method) {
      case 'transitionSession': return store.transitionSession(w, args);
      default: return (store[line.method] as (w: ActionWrite, args: never) => Promise<unknown>)(w, args);
    }
  }

  // Journaled before it applies, with the watermark it applies at, so a rebuild replays it in the same place.
  function act<T>(key: string, method: ActionMethod, args: { [field: string]: Json }): Promise<T> {
    return serial(key, async () => {
      await ensure();
      const line = { id: newId(), method, args: { key, ...args }, watermark: (await store.watermark(key)) ?? 0 };
      await journal.append(key, line);
      return (await replayAction(line)) as T;
    });
  }

  /** Applies one channel batch in seq order; a batch starting past the watermark plus one is a gap. */
  function publish(key: string, events: CatalogEvent[]): Promise<{ acked: number; gap?: true }> {
    return serial(key, async () => {
      await ensure();
      // Checked whole before anything applies: an out-of-order event would otherwise read as a replay and be dropped.
      if (events.some((e, i) => !Number.isSafeInteger(e.seq) || e.seq < 1 || (i > 0 && e.seq <= events[i - 1]!.seq))) throw new BatchError('seq must be a positive integer that strictly increases within the batch');
      const before = (await store.watermark(key)) ?? 0;
      if (events.length && events[0]!.seq > before + 1) return { acked: before, gap: true };
      for (const e of events) {
        if (e.type === 'session.created' && e.seq > ((await store.watermark(key)) ?? 0)) await journal.append(key, { incarnation: e.id });
        await applyEvent(key, e);
      }
      return { acked: (await store.watermark(key)) ?? 0 };
    });
  }

  /** Recovery: clears the session and replays its log's events with its incarnation's journaled actions between them. */
  function rebuild(key: string, events: CatalogEvent[]): Promise<void> {
    return serial(key, async () => {
      await ensure();
      const head = events.find((e) => e.type === 'session.created')?.id;
      let current: string | undefined;
      const actions: Array<Extract<JournalLine, { method: ActionMethod }>> = [];
      for (const line of await journal.read(key)) {
        if ('incarnation' in line) current = line.incarnation;
        else if (current !== undefined && current === head) actions.push(line);
      }
      actions.sort((a, b) => a.watermark - b.watermark);
      await store.resetSession({ key });
      let next = 0;
      for (const e of [...events].sort((a, b) => a.seq - b.seq)) {
        while (next < actions.length && actions[next]!.watermark < e.seq) await replayAction(actions[next++]!);
        await applyEvent(key, e);
      }
      while (next < actions.length) await replayAction(actions[next++]!);
    });
  }

  const read = async <T>(fn: () => Promise<T>) => { await ensure(); return fn(); };

  return {
    ensure,
    publish,
    rebuild,
    watermark: (key: string) => read(() => store.watermark(key)),
    rename: (key: string, title: string | null) => act<void>(key, 'setTitle', { title }),
    archive: (key: string) => act<{ applied: boolean; state?: string }>(key, 'archiveSession', {}),
    unarchive: (key: string) => act<{ applied: boolean; state?: string }>(key, 'unarchiveSession', {}),
    stopped: (key: string) => act<{ applied: boolean; state?: string }>(key, 'transitionSession', { to: 'stopped' }),
    remove: (key: string) => act<boolean>(key, 'deleteSession', {}),
    resolve: (key: string, id: string, answer: { by: Principal; choice: string; text?: string; reason?: string }) =>
      act<Awaited<ReturnType<SessionCatalogStore['resolveAsk']>>>(key, 'resolveAsk', { id, ...answer, resolvedAt: now() }),

    getSession: (key: string) => read(() => store.getSession(key)),
    getAsk: (key: string, id: string) => read(() => store.getAsk(key, id)),
    listSessions: (req: ListRequest) => read(() => store.listSessions({ where: parseQuery('sessions', req.q), orderBy: orderOf(SESSION_ORDERS, req.orderBy) as SessionOrder, ...pageOf(req) })),
    listAsks: (req: ListRequest & { session?: string }) => read(() => store.listAsks({
      where: scoped(req.session === undefined ? 'asks' : 'sessionAsks', req.q, req.session),
      orderBy: orderOf({ createdAt: req.session === undefined ? 'oldest' : 'newest' } as const, req.orderBy), ...pageOf(req),
    })),
    listArtifacts: (req: ListRequest & { session?: string }) => read(() => store.listArtifacts({
      where: scoped(req.session === undefined ? 'artifacts' : 'sessionArtifacts', req.q, req.session), orderBy: orderOf({ sealedAt: 'newest' } as const, req.orderBy), ...pageOf(req),
    })),
    close: () => store.close(),
  };
}

export type Catalog = ReturnType<typeof createCatalog>;

// The session catalog's store port (approved store-port contract v5): the core depends only on this, and the
// PostgreSQL and SQLite adapters implement it and pass one conformance suite. The contract carries the full TSDoc.

/** Any JSON value; open fields such as a resolved approval's tool arguments. Extension state never crosses this port. */
export type Json = null | boolean | number | string | Json[] | { [field: string]: Json };

/** ISO 4217 code and an int64 count of millionths; unpriced spend is 0n in the deployment's currency, never absent. */
export type Money = { currencyCode: string; amountMicros: bigint };

/** The server-set actor: a person the proxy authenticated, or the tailnet itself. */
export type Principal = { kind: 'person'; id: string } | { kind: 'tailnet' };

/** Cumulative token counts by kind since the session's start. */
export type TokenUsage = { input: number; output: number; cacheRead: number; cacheWrite: number };

/** A channel write: no-op at or below the session's watermark; above it applies, and the watermark becomes seq. */
export type EventWrite = { kind: 'event'; id: string; seq: number };

/** A write the session's log does not carry, idempotent by the core's request id; it never touches the watermark. */
export type ActionWrite = { kind: 'action'; id: string };

export type Write = EventWrite | ActionWrite;

/** running: a run is active, or it waits on a person. idle: no run, process up. stopped: the process stopped, never an
 * abort. archived: set only by the archive operation, left by invocation or unarchive. */
export type SessionState = 'running' | 'idle' | 'stopped' | 'archived';

/** Every run end sets idle, an abort included. */
export type RunOutcome = 'succeeded' | 'denied' | 'failed' | 'aborted';

export type FailedStep = { step: string; error: string };

/** One session's catalog row; dates are epoch ms. */
export type SessionRow = {
  key: string;
  agent: string;
  title?: string;
  state: SessionState;
  createdAt: number;
  lastActivity: number;
  cost: Money;
  tokens: TokenUsage;
  pendingAsks: number;
  oldestAskAt?: number;
  lastRun?: { outcome: RunOutcome; endedAt: number; failedSteps: FailedStep[] };
  /** A full-text source of the session list only, never otherwise surfaced. */
  lastMessage?: string;
};

export type AskType = 'approval' | 'question';

export type AskState = 'pending' | 'resolved' | 'cancelled';

export type AskPrompt = {
  question: string;
  options: string[];
  optionDescriptions?: Record<string, string>;
  suggested?: number | null;
  freeText: boolean;
  evidence?: string | null;
  header?: string | null;
  title?: string | null;
  detail?: string | null;
};

/** One ask; a resolved approval keeps its tool arguments as the audit trail. */
export type AskRow = {
  session: string;
  id: string;
  type: AskType;
  state: AskState;
  createdAt: number;
  resolvedAt?: number;
  resolution?: { by: Principal; choice: string; text?: string; reason?: string };
  toolCall?: { tool: string; args: Json };
  prompt?: AskPrompt;
};

/** One sealed version: the core deliverable only. The judge's verdict and an extension's links live in their own state. */
export type ArtifactRow = {
  session: string;
  kind: string;
  version: number;
  hash: string;
  content: string;
  sealedAt: number;
  summary?: string;
  /** The artifact.delivered events, in event order. */
  deliveries?: Array<{ where: string; link: string; at: number }>;
};

export type SessionField = 'key' | 'agent' | 'title' | 'state' | 'createdAt' | 'lastActivity' | 'cost.currencyCode' | 'cost.amountMicros' | 'tokens.input' | 'tokens.output' | 'tokens.cacheRead' | 'tokens.cacheWrite' | 'pendingAsks' | 'oldestAskAt' | 'lastRun.outcome';

export type AskField = 'session' | 'type' | 'state' | 'createdAt' | 'resolvedAt' | 'tool';

export type ArtifactField = 'session' | 'kind' | 'version' | 'sealedAt';

type SessionValues = {
  key: string; agent: string; title: string; state: SessionState;
  createdAt: number; lastActivity: number;
  'cost.currencyCode': string; 'cost.amountMicros': bigint;
  'tokens.input': number; 'tokens.output': number; 'tokens.cacheRead': number; 'tokens.cacheWrite': number;
  pendingAsks: number; oldestAskAt: number; 'lastRun.outcome': RunOutcome;
};
type AskValues = {
  session: string; type: AskType; state: AskState;
  createdAt: number; resolvedAt: number; tool: string;
};
type ArtifactValues = {
  session: string; kind: string; version: number;
  sealedAt: number;
};

// A leaf's value type follows its field; prefix and wildcard leaves exist only on the collection's text fields.
type LeafOf<V, F extends string, T extends string> = F extends keyof V
  ? { kind: 'match'; field: F; value: V[F] }
    | { kind: 'prefix'; field: F & T; value: string }
    | { kind: 'wildcard'; field: F & T; pattern: string }
    | { kind: 'compare'; field: F; op: 'gt' | 'gte' | 'lt' | 'le'; value: V[F] }
    | { kind: 'range'; field: F; from?: { value: V[F]; inclusive: boolean }; to?: { value: V[F]; inclusive: boolean } }
  : never;
type SessionLeaf<F extends string> = LeafOf<SessionValues, F, 'key' | 'agent' | 'title'>;
type AskLeaf<F extends string> = LeafOf<AskValues, F, 'session' | 'tool'>;
type ArtifactLeaf<F extends string> = LeafOf<ArtifactValues, F, 'session' | 'kind'>;
type FieldLeaf<F extends string> =
  | ([F] extends [SessionField] ? SessionLeaf<F> : never)
  | ([F] extends [AskField] ? AskLeaf<F> : never)
  | ([F] extends [ArtifactField] ? ArtifactLeaf<F> : never);

/** The parsed q of one list, parsed and field-checked in the core; an absent or empty q is { kind: 'all' }.
 * A wildcard pattern keeps Lucene's backslash escapes: an unescaped * or ? is the wildcard. */
export type QueryAst<F extends string> =
  | { kind: 'all' }
  | { kind: 'and'; terms: QueryAst<F>[] }
  | { kind: 'or'; terms: QueryAst<F>[] }
  | { kind: 'not'; term: QueryAst<F> }
  /** A bare term or phrase: full text over the collection's documented text fields. */
  | { kind: 'text'; value: string; phrase?: boolean; wildcard?: boolean }
  | FieldLeaf<F>;

export type SessionOrder =
  | { field: 'lastActivity'; direction: 'newest' }
  | { field: 'createdAt'; direction: 'newest' }
  | { field: 'oldestAskAt'; direction: 'oldest' };

export type AskOrder = { field: 'createdAt'; direction: 'newest' | 'oldest' };

export type ArtifactOrder = { field: 'sealedAt'; direction: 'newest' };

/** One list request; cursor is a previous page's, bound to the (where, orderBy) that issued it. */
export type ListOptions<F extends string, O extends { field: string; direction: 'newest' | 'oldest' }> = {
  where: QueryAst<F>;
  orderBy: O;
  pageSize: number;
  cursor?: string;
};

/** One keyset page; cursor is present while more rows remain. */
export type ListPage<T> = { rows: T[]; cursor?: string };

/** The port. Every write applies its row and derived counts in one transaction, and a refused write changes nothing;
 * reads are pure and never start a session's process. */
export interface SessionCatalogStore {
  migrate(): Promise<void>;
  close(): Promise<void>;

  /** session.created, from the running process: the row starts idle. Under another incarnation's tombstone it starts fresh. */
  createSession(write: EventWrite, row: { key: string; agent: string; createdAt: number }): Promise<void>;
  /** The highest applied seq; undefined when the store knows no such key. */
  watermark(key: string): Promise<number | undefined>;
  /** Moves the watermark past an event the catalog does not apply (an extension event, a stopped or archived echo).
   * Port v5 names no key; the session's key is the second argument, as on every other write. */
  advanceWatermark(write: EventWrite, change: { key: string }): Promise<void>;
  /** running from idle, stopped or archived; idle from running or stopped, never archived; stopped from running or idle. */
  transitionSession(write: EventWrite, change: { key: string; to: 'running' | 'idle' }): Promise<{ applied: boolean; state?: SessionState }>;
  transitionSession(write: ActionWrite, change: { key: string; to: 'stopped' }): Promise<{ applied: boolean; state?: SessionState }>;
  /** title.set (the derived default) or a person's rename; null restores the default. */
  setTitle(write: Write, change: { key: string; title: string | null }): Promise<void>;
  /** From idle or stopped; refused while running, a no-op when already archived. */
  archiveSession(write: ActionWrite, change: { key: string }): Promise<{ applied: boolean; state?: SessionState }>;
  /** Archived back to stopped; a no-op otherwise. */
  unarchiveSession(write: ActionWrite, change: { key: string }): Promise<{ applied: boolean; state?: SessionState }>;
  /** run.ended: lastRun, lastActivity and lastMessage together; no state, spend or ask moves. */
  recordRunOutcome(write: EventWrite, outcome: {
    key: string;
    outcome: RunOutcome;
    endedAt: number;
    failedSteps: FailedStep[];
    lastMessage?: string;
  }): Promise<void>;
  /** spend.recorded: cumulative totals; another currency or a lower total is refused with the watermark unchanged. */
  recordSpend(write: EventWrite, spend: {
    key: string;
    cost: Money;
    tokens: TokenUsage;
  }): Promise<{ applied: boolean; refused?: 'currency' | 'regressed' }>;
  /** A person's delete: rows and idempotency state go, a tombstone keeps the incarnation; false for absent. */
  deleteSession(write: ActionWrite, change: { key: string }): Promise<boolean>;
  /** The rebuild's clear: rows, watermark, action ids and any tombstone; never a user delete. */
  resetSession(change: { key: string }): Promise<void>;
  getSession(key: string): Promise<SessionRow | undefined>;
  listSessions(options: ListOptions<SessionField, SessionOrder>): Promise<ListPage<SessionRow>>;

  /** ask.opened: the ask, pendingAsks and oldestAskAt together; a session not running becomes running. */
  openAsk(write: EventWrite, ask: {
    key: string;
    id: string;
    type: AskType;
    createdAt: number;
    toolCall?: { tool: string; args: Json };
    prompt?: AskPrompt;
  }): Promise<void>;
  /** Applies only while pending; by is stored as given. */
  resolveAsk(write: Write, change: {
    key: string;
    id: string;
    by: Principal;
    choice: string;
    text?: string;
    reason?: string;
    resolvedAt: number;
  }): Promise<{ applied: boolean; ask?: AskRow }>;
  /** ask.cancelled: applies only while pending. */
  cancelAsk(write: EventWrite, change: { key: string; id: string }): Promise<void>;
  getAsk(key: string, id: string): Promise<AskRow | undefined>;
  listAsks(options: ListOptions<AskField, AskOrder>): Promise<ListPage<AskRow>>;

  /** artifact.sealed: the only write that creates an artifact row; a higher seq supersedes it in place. */
  sealArtifact(write: EventWrite, artifact: {
    key: string;
    kind: string;
    version: number;
    hash: string;
    content: string;
    sealedAt: number;
    summary?: string;
  }): Promise<void>;
  /** artifact.delivered: appends one delivery, once per event id whatever its seq; a no-op when the version is absent. */
  recordArtifactDelivery(write: EventWrite, delivery: {
    key: string;
    kind: string;
    version: number;
    where: string;
    link: string;
    at: number;
  }): Promise<void>;
  listArtifacts(options: ListOptions<ArtifactField, ArtifactOrder>): Promise<ListPage<ArtifactRow>>;
}

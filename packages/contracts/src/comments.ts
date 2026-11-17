// Review comments on the version up for approval and the reviews that send them (Contracts app API v13), in the app's own database.
import { randomBytes } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { DatabaseSync } from 'node:sqlite';
import { sendProblem, serviceHttp, type PageRoute } from '@quack/sdk';
import { anchoredText, anchorOf } from './anchors.ts';
import { parseFilter, QueryError } from './filter.ts';
import { BadRequest, cursorOf, sizeOf, tokenOf } from './paging.ts';
import { OPEN_FENCE, reviewReason, type Anchor } from './reason.ts';
import { MAX_REPLIES, REPLY_KIND } from './reply.ts';
import { CONTRACT_KIND, parseSpec, type ContractSpec } from './spec.ts';

export type Principal = { kind: 'person'; id: string } | { kind: 'tailnet' };
export type CommentState = 'draft' | 'sent' | 'answered' | 'addressed' | 'resolved';
type ReviewState = 'prepared' | 'sent' | 'released';
/** What the app reads of a contract session from the catalog: its contract.* artifacts with content, and its propose_contract asks. */
export type Facts = { artifacts: Array<{ kind: string; version: number; content?: string; sealedAt?: string }>; asks: Array<{ id: string; state: string; resolvedAt?: string; resolution?: { choice: string; reason?: string }; toolCall?: { tool: string; args: unknown } }> };

type Row = { id: string; key: string; artifact: number; version: number; anchor: string; quote: string; body: string; author: string; created_at: number; resolved_at: number | null };
type ReviewRow = { id: string; key: string; artifact: number; ask_id: string; note: string | null; comments: string; by: string; prepared_at: number; reason: string; released: number };
type Review = ReviewRow & { state: ReviewState; ids: string[] };

const LIMITS = { body: 1000, note: 2000, quote: 1000, perReview: 20, drafts: 100, reasonBytes: 60_000 };
const STATES = ['draft', 'sent', 'answered', 'addressed', 'resolved'] as const;
const points = (s: string) => [...s].length;
const iso = (ms: number) => new Date(ms).toISOString();
const who = (p: Principal) => JSON.stringify(p.kind === 'person' ? { kind: 'person', id: p.id } : { kind: 'tailnet' });

/** A refusal answered with its status and detail as a problem. */
class Refused extends Error { status: number; constructor(status: number, message: string) { super(message); this.status = status; } }
const refuse = (status: number, message: string): never => { throw new Refused(status, message); };

// The gateway hands the app its headers as received: a launch credential is a session, the proxy's named person a person, else the tailnet.
function callerOf(req: IncomingMessage): Principal | undefined {
  if (req.headers.authorization !== undefined) return undefined;
  const name = req.headers['x-authentik-username'];
  return typeof name === 'string' && name ? { kind: 'person', id: name } : { kind: 'tailnet' };
}

function openStore(path: string) {
  const db = new DatabaseSync(path);
  db.exec(`CREATE TABLE IF NOT EXISTS comments (id TEXT PRIMARY KEY, key TEXT NOT NULL, artifact INTEGER NOT NULL, version INTEGER NOT NULL, anchor TEXT NOT NULL,
      quote TEXT NOT NULL, body TEXT NOT NULL, author TEXT NOT NULL, created_at INTEGER NOT NULL, resolved_at INTEGER);
    CREATE INDEX IF NOT EXISTS comments_key ON comments (key, created_at, id);
    CREATE TABLE IF NOT EXISTS reviews (id TEXT PRIMARY KEY, key TEXT NOT NULL, artifact INTEGER NOT NULL, ask_id TEXT NOT NULL, note TEXT, comments TEXT NOT NULL,
      by TEXT NOT NULL, prepared_at INTEGER NOT NULL, reason TEXT NOT NULL, released INTEGER NOT NULL DEFAULT 0);
    CREATE INDEX IF NOT EXISTS reviews_key ON reviews (key, prepared_at, id);`);
  return {
    db,
    comments: (key: string) => db.prepare('SELECT * FROM comments WHERE key = ? ORDER BY created_at, id').all(key) as unknown as Row[],
    reviews: (key: string) => db.prepare('SELECT * FROM reviews WHERE key = ? ORDER BY prepared_at, id').all(key) as unknown as ReviewRow[],
    insert: (r: Row) => db.prepare('INSERT INTO comments (id, key, artifact, version, anchor, quote, body, author, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)').run(r.id, r.key, r.artifact, r.version, r.anchor, r.quote, r.body, r.author, r.created_at),
    edit: (id: string, body: string) => db.prepare('UPDATE comments SET body = ? WHERE id = ?').run(body, id),
    remove: (id: string) => db.prepare('DELETE FROM comments WHERE id = ?').run(id),
    resolve: (id: string, at: number) => db.prepare('UPDATE comments SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL').run(at, id),
    prepare: (r: Omit<ReviewRow, 'released'>) => db.prepare('INSERT INTO reviews (id, key, artifact, ask_id, note, comments, by, prepared_at, reason) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)').run(r.id, r.key, r.artifact, r.ask_id, r.note, r.comments, r.by, r.prepared_at, r.reason),
    release: (id: string) => db.prepare('UPDATE reviews SET released = 1 WHERE id = ?').run(id),
  };
}
type Store = ReturnType<typeof openStore>;

// A review's state is read from its ask on every read, never a clock: sent only on a rejection with exactly its reason.
function reviewState(r: ReviewRow, asks: Map<string, Facts['asks'][number]>): ReviewState {
  const ask = asks.get(r.ask_id);
  if (r.released) return 'released';
  if (ask?.state === 'pending') return 'prepared';
  return ask?.state === 'resolved' && ask.resolution?.choice === 'reject' && ask.resolution.reason === r.reason ? 'sent' : 'released';
}

const pendingAsk = (facts: Facts, artifact: number) =>
  facts.asks.find((a) => a.state === 'pending' && a.toolCall?.tool === 'propose_contract' && Number((a.toolCall.args as { artifact?: unknown } | null)?.artifact) === artifact);
const specOf = (content: string | undefined): Partial<ContractSpec> => { const s = parseSpec(content ?? ''); return typeof s === 'string' ? {} : s; };
const parsed = (content: string | undefined): Record<string, unknown> => { try { return JSON.parse(content ?? '') ?? {}; } catch { return {}; } };

/** One contract's comments and reviews joined with its catalog facts, read once per request. */
function viewOf(store: Store, key: string, facts: Facts) {
  const asks = new Map(facts.asks.map((a) => [a.id, a]));
  const specs = facts.artifacts.filter((a) => a.kind === CONTRACT_KIND).sort((a, b) => a.version - b.version).map((a) => ({ n: a.version, spec: specOf(a.content) }));
  const replies = facts.artifacts.filter((a) => a.kind === REPLY_KIND).sort((a, b) => a.version - b.version).map((a) => ({ comment: parsed(a.content).comment, text: parsed(a.content).text, at: a.sealedAt ?? '' }));
  const reviews: Review[] = store.reviews(key).map((r) => ({ ...r, state: reviewState(r, asks), ids: JSON.parse(r.comments) as string[] }));
  const memo = new Map<string, string | undefined>();
  const textIn = (n: number, anchor: string) => {
    const k = `${n}\n${anchor}`;
    if (!memo.has(k)) memo.set(k, anchoredText(specs.find((s) => s.n === n)?.spec ?? {}, JSON.parse(anchor) as Anchor));
    return memo.get(k);
  };
  return { facts, asks, specs, replies, reviews, textIn, rows: store.comments(key) };
}
type View = ReturnType<typeof viewOf>;

const addresses = (spec: Partial<ContractSpec>, id: string) => (Array.isArray(spec.changes) ? spec.changes : []).some((r) => Array.isArray(r?.addresses) && r.addresses.includes(id));

// Outdated against the newest sealed spec: changed or removed there, `in` the first later artifact where that showed.
function outdatedOf(v: View, row: Row, own: string | undefined) {
  const later = v.specs.filter((s) => s.n > row.artifact);
  const newest = later.at(-1);
  const now = newest && v.textIn(newest.n, row.anchor);
  if (!newest || now === own) return undefined;
  const mark = now === undefined ? 'removed' : 'changed';
  const first = later.find((s) => (mark === 'removed' ? v.textIn(s.n, row.anchor) === undefined : v.textIn(s.n, row.anchor) !== own))!;
  return { mark, in: first.n };
}

function commentOf(v: View, row: Row) {
  const review = v.reviews.findLast((r) => r.state !== 'released' && r.ids.includes(row.id));
  const sent = review?.state === 'sent';
  const own = v.textIn(row.artifact, row.anchor);
  const replies = sent ? v.replies.filter((r) => r.comment === row.id && typeof r.text === 'string').slice(0, MAX_REPLIES).map((r) => ({ text: r.text as string, at: r.at })) : [];
  const addressedIn = sent ? v.specs.find((s) => s.n > row.artifact && addresses(s.spec, row.id) && v.textIn(s.n, row.anchor) !== own)?.n : undefined;
  const outdated = outdatedOf(v, row, own);
  const state: CommentState = row.resolved_at !== null ? 'resolved' : !sent ? 'draft' : addressedIn ? 'addressed' : replies.length ? 'answered' : 'sent';
  const sentAt = sent ? v.asks.get(review.ask_id)?.resolvedAt : undefined;
  return {
    id: row.id, artifact: row.artifact, version: row.version, anchor: JSON.parse(row.anchor) as Anchor, quote: row.quote, body: row.body, author: JSON.parse(row.author) as Principal, state,
    createdAt: iso(row.created_at), ...(sentAt && { sentAt }), ...(review && { review: review.id }), replies,
    ...(addressedIn && { addressedIn }), ...(outdated && { outdated }), ...(row.resolved_at !== null && { resolvedAt: iso(row.resolved_at) }),
  };
}
export type Comment = ReturnType<typeof commentOf>;

const reviewOf = (r: Review) => ({ id: r.id, artifact: r.artifact, askId: r.ask_id, ...(r.note !== null && { note: r.note }), comments: r.ids, by: JSON.parse(r.by) as Principal, preparedAt: iso(r.prepared_at), state: r.state, reason: r.reason });

const bodyText = (v: unknown) => (typeof v === 'string' && points(v) >= 1 && points(v) <= LIMITS.body ? v : refuse(400, `body must be 1 to ${LIMITS.body} characters`));

// Paths are relative to the mount; ids are a prefix plus base64url, so `:resolve` cannot be part of one.
const ROUTE = /^\/contracts\/([^/]+)\/(comments|reviews)(?:\/([\w-]+)(:resolve)?)?$/;
/** The comment and review routes, each a page route the gateway admits credential-free. */
export const COMMENT_PAGES: PageRoute[] = [['GET', ''], ['POST', ''], ['PATCH', '/[\\w-]+'], ['DELETE', '/[\\w-]+'], ['POST', '/[\\w-]+:resolve']].flatMap(([method, tail]) => [{ method: method!, path: new RegExp(`^/contracts/[^/]+/comments${tail}$`) }])
  .concat([{ method: 'POST', path: /^\/contracts\/[^/]+\/reviews$/ }, { method: 'GET', path: /^\/contracts\/[^/]+\/reviews\/[\w-]+$/ }, { method: 'DELETE', path: /^\/contracts\/[^/]+\/reviews\/[\w-]+$/ }]);

type Seams = { dbPath: string; facts: (key: string) => Promise<Facts | undefined>; now?: () => number; newId?: (prefix: string) => string };
type Call = { req: IncomingMessage; url: URL; key: string; me: Principal; mine: string; store: Store; view: View; now: number; newId: (prefix: string) => string };

const http = serviceHttp({ maxBody: 64 * 1024 });
async function bodyOf(req: IncomingMessage): Promise<Record<string, unknown>> {
  const b = await http.json(req).catch((err: unknown) => refuse(400, err instanceof Error && err.message === 'body too large' ? 'the body is over 64 KiB' : 'the body must be JSON'));
  return typeof b === 'object' && b !== null && !Array.isArray(b) ? (b as Record<string, unknown>) : refuse(400, 'the body must be a JSON object');
}

function mineOnly(c: Call, row: Pick<Row, 'author'> | undefined, what: string) {
  if (!row) refuse(404, `no such ${what}`);
  if (row!.author !== c.mine) refuse(403, `only its author may change this ${what}`);
}
const rowIn = (c: Call, id: string) => c.view.rows.find((r) => r.id === id);
// A held or sent comment no longer edits, deletes or goes into another review.
function draftOnly(c: Call, row: Row) {
  const shown = commentOf(c.view, row);
  if (shown.state !== 'draft') refuse(409, `comment ${row.id} is sent, so it is kept as it was sent`);
  if (shown.review) refuse(409, `comment ${row.id} is held by prepared review ${shown.review} until that review is sent or released`);
  return shown;
}

function list(c: Call) {
  const params = c.url.searchParams;
  const pageSize = sizeOf(params.get('pageSize'));
  const q = params.get('q') ?? '';
  const { test } = parseFilter(q, { artifact: 'integer', state: STATES });
  const after = cursorOf(params.get('pageToken'), q);
  const all = c.view.rows.map((r) => ({ r, c: commentOf(c.view, r) })).filter((x) => (x.c.state !== 'draft' || x.r.author === c.mine) && test(x.c));
  const start = after ? all.findIndex((x) => `${x.r.created_at}|${x.r.id}` === after) + 1 : 0;
  if (after && start === 0) throw new BadRequest('pageToken names no comment of this list');
  const page = all.slice(start, start + pageSize);
  const last = page.at(-1)?.r;
  return { comments: page.map((x) => x.c), caller: c.me, ...(start + pageSize < all.length && last && { nextPageToken: tokenOf(`${last.created_at}|${last.id}`, q) }) };
}

async function create(c: Call) {
  const b = await bodyOf(c.req);
  const artifact = Number.isInteger(b.artifact) && (b.artifact as number) >= 1 ? (b.artifact as number) : refuse(400, 'artifact must be the contract.spec artifact number');
  const anchor = anchorOf(b.anchor);
  if (typeof anchor === 'string') return refuse(400, anchor);
  const body = bodyText(b.body);
  if (!pendingAsk(c.view.facts, artifact)) refuse(409, `no pending approval names artifact ${artifact}: comments are made only on the version up for approval`);
  const spec = c.view.specs.find((s) => s.n === artifact);
  const text = spec && anchoredText(spec.spec, anchor);
  if (text === undefined || (anchor.on === 'guarantee' || anchor.on === 'nonGoal' ? text !== anchor.text : false)) refuse(400, `the anchor does not resolve in artifact ${artifact}`);
  const drafts = c.view.rows.filter((r) => r.author === c.mine && r.artifact === artifact && commentOf(c.view, r).state === 'draft').length;
  if (drafts >= LIMITS.drafts) refuse(400, `you have ${LIMITS.drafts} drafts on this version, the most it takes; send or delete some first`);
  const row: Row = { id: c.newId('cmt_'), key: c.key, artifact, version: Number(spec!.spec.version) || 1, anchor: JSON.stringify(anchor), quote: [...text!].slice(0, LIMITS.quote).join(''), body, author: c.mine, created_at: c.now, resolved_at: null };
  c.store.insert(row);
  return commentOf({ ...c.view, rows: [...c.view.rows, row] }, row);
}

async function edit(c: Call, id: string) {
  const row = rowIn(c, id);
  mineOnly(c, row, 'comment');
  const body = bodyText((await bodyOf(c.req)).body);
  draftOnly(c, row!);
  c.store.edit(id, body);
  return commentOf(c.view, { ...row!, body });
}

function resolve(c: Call, id: string) {
  const row = rowIn(c, id);
  mineOnly(c, row, 'comment');
  if (commentOf(c.view, row!).state === 'draft') refuse(409, `comment ${id} is not sent, so there is nothing to resolve`);
  c.store.resolve(id, c.now);
  return commentOf(c.view, { ...row!, resolved_at: row!.resolved_at ?? c.now });
}

function noteOf(v: unknown): string | undefined {
  if (v === undefined) return undefined;
  if (typeof v !== 'string' || points(v) > LIMITS.note) return refuse(400, `note must be text of at most ${LIMITS.note} characters`);
  return v.includes(OPEN_FENCE) ? refuse(400, `note must not contain ${OPEN_FENCE}, which marks the review's comments`) : v || undefined;
}

function idsOf(v: unknown): string[] {
  const ok = Array.isArray(v) && v.length >= 1 && v.length <= LIMITS.perReview && v.every((x) => typeof x === 'string') && new Set(v).size === v.length;
  return ok ? (v as string[]) : refuse(400, `comments must list 1 to ${LIMITS.perReview} distinct draft ids`);
}

async function prepare(c: Call) {
  const b = await bodyOf(c.req);
  const note = noteOf(b.note);
  const rows = idsOf(b.comments).map((id) => {
    const row = rowIn(c, id);
    return row && row.author === c.mine && commentOf(c.view, row).state === 'draft' ? row : refuse(400, `${id} is not one of your drafts`);
  });
  rows.forEach((r) => draftOnly(c, r));
  const artifact = rows[0]!.artifact;
  const ask = rows.every((r) => r.artifact === artifact) ? pendingAsk(c.view.facts, artifact) : undefined;
  if (!ask) return refuse(409, 'no pending approval names the version these drafts are on, so there is nothing to ask changes of');
  const id = c.newId('rev_');
  const reason = reviewReason(note, { review: id, artifact, comments: rows.map((r) => ({ id: r.id, anchor: JSON.parse(r.anchor) as Anchor, body: r.body })) });
  if (Buffer.byteLength(JSON.stringify(reason)) > LIMITS.reasonBytes) refuse(400, `the review's reason is over ${LIMITS.reasonBytes} bytes as JSON; send fewer or shorter comments`);
  const row = { id, key: c.key, artifact, ask_id: ask.id, note: note ?? null, comments: JSON.stringify(rows.map((r) => r.id)), by: c.mine, prepared_at: c.now, reason };
  c.store.prepare(row);
  return reviewOf({ ...row, released: 0, state: 'prepared', ids: rows.map((r) => r.id) });
}

function reviewIn(c: Call, id: string) {
  const r = c.view.reviews.find((x) => x.id === id);
  mineOnly(c, r && { author: r.by }, 'review');
  return r!;
}

function removeDraft(c: Call, id: string): [number, unknown] {
  const row = rowIn(c, id);
  mineOnly(c, row, 'comment');
  draftOnly(c, row!);
  c.store.remove(id);
  return [204, undefined];
}

function release(c: Call, id: string): [number, unknown] {
  const r = reviewIn(c, id);
  if (r.state !== 'prepared') refuse(409, `review ${id} is ${r.state}, so it cannot be released`);
  c.store.release(id);
  return [204, undefined];
}

// Each route by method and shape: `id` stands for a comment or review id, `:resolve` for the resolve action.
const HANDLERS: Record<string, (c: Call, id: string) => [number, unknown] | Promise<[number, unknown]>> = {
  'GET comments': (c) => [200, list(c)],
  'POST comments': async (c) => [201, await create(c)],
  'PATCH comments/id': async (c, id) => [200, await edit(c, id)],
  'DELETE comments/id': removeDraft,
  'POST comments/id:resolve': (c, id) => [200, resolve(c, id)],
  'POST reviews': async (c) => [201, await prepare(c)],
  'GET reviews/id': (c, id) => [200, reviewOf(reviewIn(c, id))],
  'DELETE reviews/id': release,
};

/** The comment and review routes; `handle` answers false for any other path. */
export function commentRoutes(seams: Seams) {
  const store = openStore(seams.dbPath);
  const now = seams.now ?? Date.now;
  const newId = seams.newId ?? ((prefix: string) => `${prefix}${randomBytes(16).toString('base64url')}`);
  async function answer(req: IncomingMessage, url: URL, m: RegExpExecArray): Promise<[number, unknown]> {
    const handler = HANDLERS[`${req.method} ${m[2]}${m[3] ? '/id' : ''}${m[4] ?? ''}`];
    if (!handler) return refuse(404, 'not found');
    const me = callerOf(req);
    if (!me) return refuse(403, 'a session may not read or write review comments; only a person or the tailnet may');
    let key = '';
    try { key = decodeURIComponent(m[1]!); } catch { /* reads as no key */ }
    const facts = key.startsWith('contracts:') ? await seams.facts(key) : undefined;
    if (!facts) return refuse(404, 'no such contract');
    const c: Call = { req, url, key, me, mine: who(me), store, view: viewOf(store, key, facts), now: now(), newId };
    return handler(c, m[3] ?? '');
  }
  return {
    async handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<boolean> {
      const m = ROUTE.exec(url.pathname);
      if (!m) return false;
      try {
        const [status, body] = await answer(req, url, m);
        if (status === 204) res.writeHead(204).end();
        else http.send(res, status, body);
      } catch (err) {
        if (err instanceof Refused) sendProblem(res, err.status, err.message);
        else if (err instanceof BadRequest || err instanceof QueryError) sendProblem(res, 400, err.message);
        else sendProblem(res, 503, `the catalog is unavailable: ${err instanceof Error ? err.message : String(err)}`);
      }
      return true;
    },
    close: () => store.db.close(),
  };
}

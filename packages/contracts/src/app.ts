// The contracts extension's own read: each Contract Studio session's contract, from its sealed artifacts in the catalog.
import type { IncomingMessage, ServerResponse } from 'node:http';
import { escapeTerm, sendProblem, serviceHttp, type App, type AppContext } from '@quack/sdk';
import { APPROVED_KIND, CHANGES_KIND, DISCARDED_KIND } from './approval.ts';
import { commentRoutes, type Facts } from './comments.ts';
import { parseFilter, QueryError } from './filter.ts';
import { BadRequest, cursorOf, sizeOf, tokenOf } from './paging.ts';
import { CONTRACT_KIND, SPEC_KINDS } from './spec.ts';

/** One contract as the Contracts page shows it. Versions are the contract's own spec versions, not artifact numbers. */
export type ContractRecord = {
  key: string; title: string; kind: string;
  /** The newest drafted spec version. */ draft?: number;
  /** The newest version put to the person for approval. */ proposed?: number;
  approved?: number; changesRequested?: number; pendingApproval: boolean;
};

/** Where one sealed spec stands with the person: never put to them, put up, approved, or sent back with changes requested. */
export type VersionStatus = 'draft' | 'proposed' | 'approved' | 'changes_requested';
/** One sealed `contract.spec` artifact: its artifact number, the spec version it holds, and its status. */
export type SpecVersion = { artifact: number; version: number; status: VersionStatus };

type Artifact = { kind: string; version: number; content?: string };
type Ask = { state: string; resolution?: { choice: string }; toolCall?: { tool: string; args: unknown } };

const { send } = serviceHttp();
/** The routes the page reads: every contract, and one contract by its session key. */
export const PAGE = /^\/contracts(?:\/[^/]+)?$/;
const ONE = /^\/contracts\/([^/]+)$/;
const keyOf = (path: string) => { try { return decodeURIComponent(ONE.exec(path)?.[1] ?? ''); } catch { return ''; } };
// A filter that matches little would otherwise walk the whole catalog in one request; a short page with a token is allowed.
const MAX_CATALOG_PAGES = 10;
const parse =(content: string | undefined): Record<string, unknown> => { try { return JSON.parse(content ?? '') ?? {}; } catch { return {}; } };
const newest = (all: Artifact[], kind: string) => all.filter((a) => a.kind === kind).sort((a, b) => a.version - b.version).at(-1);
const versionOf = (a: Artifact | undefined) => (typeof parse(a?.content).version === 'number' ? (parse(a?.content).version as number) : undefined);

/** A contract from its session's sealed artifacts and its propose_contract asks; the core row carries none of it. */
export function contractOf(key: string, fallbackTitle: string | undefined, artifacts: Artifact[], asks: Ask[]): ContractRecord {
  const latest = newest(artifacts, CONTRACT_KIND);
  const dropped = latest && artifacts.filter((a) => a.kind === DISCARDED_KIND).map((a) => parse(a.content)).findLast((d) => d.artifact === latest.version);
  const current = dropped && typeof dropped.restored === 'number' ? artifacts.find((a) => a.kind === CONTRACT_KIND && a.version === dropped.restored) ?? latest : latest;
  const spec = parse(current?.content);
  const proposals = asks.filter((a) => a.toolCall?.tool === 'propose_contract');
  const proposed = Math.max(...proposals.map((a) => Number((a.toolCall?.args as { version?: unknown } | null)?.version) || 0));
  return {
    key, title: typeof spec.title === 'string' ? spec.title : (fallbackTitle ?? 'New contract'), kind: (SPEC_KINDS as readonly unknown[]).includes(spec.kind) ? (spec.kind as string) : 'rest',
    ...(typeof spec.version === 'number' && { draft: spec.version }), ...(proposed > 0 && { proposed }),
    ...(versionOf(newest(artifacts, APPROVED_KIND)) !== undefined && { approved: versionOf(newest(artifacts, APPROVED_KIND)) }),
    ...(versionOf(newest(artifacts, CHANGES_KIND)) !== undefined && { changesRequested: versionOf(newest(artifacts, CHANGES_KIND)) }),
    pendingApproval: proposals.some((a) => a.state === 'pending'),
  };
}

const proposedArtifact = (a: Ask) => Number((a.toolCall?.args as { artifact?: unknown } | null)?.artifact) || undefined;

// Each decision the person made, by the artifact it decided: a record names its artifact (an older one only its spec version, so the
// newest artifact of that version), and an answered proposal its own, which covers a decision whose record is not sealed yet.
function decided(specs: SpecVersion[], artifacts: Artifact[], asks: Ask[], kind: string, choice: string): Set<number> {
  const named = artifacts.filter((a) => a.kind === kind).map((a) => parse(a.content)).map((r) => Number(r.artifact) || specs.findLast((s) => s.version === r.version)?.artifact);
  const answered = asks.filter((a) => a.state === 'resolved' && a.resolution?.choice === choice).map(proposedArtifact);
  return new Set([...named, ...answered].filter((n): n is number => n !== undefined));
}

/** Every sealed spec, oldest first, with where it stands: from the session's approval records and propose_contract asks, never a verdict. */
export function versionsOf(artifacts: Artifact[], all: Ask[]): SpecVersion[] {
  const asks = all.filter((a) => a.toolCall?.tool === 'propose_contract');
  const specs = artifacts.filter((a) => a.kind === CONTRACT_KIND).sort((a, b) => a.version - b.version)
    .map((a): SpecVersion => ({ artifact: a.version, version: Number(parse(a.content).version) || 0, status: 'draft' }));
  const approved = decided(specs, artifacts, asks, APPROVED_KIND, 'approve');
  const changes = decided(specs, artifacts, asks, CHANGES_KIND, 'reject');
  const proposed = new Set(asks.map(proposedArtifact));
  const statusOf = (n: number): VersionStatus => (approved.has(n) ? 'approved' : changes.has(n) ? 'changes_requested' : proposed.has(n) ? 'proposed' : 'draft');
  return specs.map((s) => ({ ...s, status: statusOf(s.artifact) }));
}

/** The app: GET /contracts reads every live Contract Studio session, and GET /contracts/{key} one with its versions, through the catalog, never starting a process;
 * the comment and review routes keep review comments in the app's own database. `now` and `newId` are seams for tests. */
export function contractsApp(ctx: AppContext, seams: { now?: () => number; newId?: (prefix: string) => string } = {}): App {
  async function all<T>(path: string, field: string): Promise<T[]> {
    const rows: T[] = [];
    for (let token = ''; ;) {
      const res = await ctx.fetch(`${ctx.distributorUrl}${path}${path.includes('?') ? '&' : '?'}pageSize=1000${token && `&pageToken=${encodeURIComponent(token)}`}`);
      if (!res.ok) throw new Error(`${path} answered ${res.status}`);
      const page = (await res.json()) as Record<string, T[]> & { nextPageToken?: string };
      rows.push(...(page[field] ?? []));
      if (!page.nextPageToken) return rows;
      token = page.nextPageToken;
    }
  }
  async function records(key: string) {
    const base = `/sessions/${encodeURIComponent(key)}`;
    const [artifacts, asks] = await Promise.all([all<Artifact>(`${base}/artifacts?q=${encodeURIComponent('kind:contract.*')}`, 'artifacts'), all<Ask>(`${base}/asks?q=${encodeURIComponent('tool:propose_contract')}`, 'asks')]);
    return { artifacts, asks };
  }
  // One catalog page of live contract sessions; createdAt never moves (ties fall to the key), so the keyset holds while sessions run.
  async function sessions(q: string, pageSize: number, token: string) {
    const res = await ctx.fetch(`${ctx.distributorUrl}/sessions?q=${encodeURIComponent(q)}&orderBy=createdAt&pageSize=${pageSize}${token && `&pageToken=${encodeURIComponent(token)}`}`);
    if (res.status === 400) throw new BadRequest(String(((await res.json().catch(() => ({}))) as { detail?: unknown }).detail ?? 'bad request'));
    if (!res.ok) throw new Error(`/sessions answered ${res.status}`);
    return (await res.json()) as { sessions?: Array<{ key: string; title?: string }>; nextPageToken?: string };
  }
  // Fills one page across catalog pages, asking each for only what is still missing, so the next cursor is the catalog's own.
  async function list(params: URLSearchParams): Promise<{ contracts: ContractRecord[]; nextPageToken?: string }> {
    const pageSize = sizeOf(params.get('pageSize'));
    const asked = params.get('q') ?? '';
    const { test, pushed } = parseFilter(asked);
    const q = [`key:${escapeTerm('contracts:')}*`, '-state:archived', ...pushed.map((p) => `(${p})`)].join(' AND ');
    const contracts: ContractRecord[] = [];
    let cursor = cursorOf(params.get('pageToken'), asked);
    for (let walked = 0; walked < MAX_CATALOG_PAGES && contracts.length < pageSize; walked++) {
      const page = await sessions(q, pageSize - contracts.length, cursor);
      const read = await Promise.all((page.sessions ?? []).map(async (s) => { const r = await records(s.key); return contractOf(s.key, s.title, r.artifacts, r.asks); }));
      contracts.push(...read.filter(test));
      cursor = page.nextPageToken ?? '';
      if (!cursor) break;
    }
    return { contracts, ...(cursor && { nextPageToken: tokenOf(cursor, asked) }) };
  }
  // A session's catalog row, or undefined when the catalog has no such session.
  async function session(key: string): Promise<{ title?: string } | undefined> {
    const row = await ctx.fetch(`${ctx.distributorUrl}/sessions/${encodeURIComponent(key)}`);
    if (row.status === 404) return undefined;
    if (!row.ok) throw new Error(`/sessions/${key} answered ${row.status}`);
    return (await row.json()) as { title?: string };
  }
  // One contract with every spec version's status; undefined when the catalog has no such session.
  async function one(key: string): Promise<(ContractRecord & { versions: SpecVersion[] }) | undefined> {
    const row = await session(key);
    if (!row) return undefined;
    const { artifacts, asks } = await records(key);
    return { ...contractOf(key, row.title, artifacts, asks), versions: versionsOf(artifacts, asks) };
  }
  const comments = commentRoutes({ ...seams, dbPath: ctx.dbPath, facts: async (key) => ((await session(key)) ? ((await records(key)) as Facts) : undefined) });
  async function route(url: URL, res: ServerResponse) {
    if (url.pathname === '/contracts') return send(res, 200, await list(url.searchParams));
    const key = keyOf(url.pathname);
    const found = key.startsWith('contracts:') ? await one(key) : undefined;
    return found ? send(res, 200, found) : sendProblem(res, 404, 'no such contract');
  }
  return {
    async handle(req: IncomingMessage, res: ServerResponse) {
      const url = new URL(req.url ?? '', 'http://x');
      if (await comments.handle(req, res, url)) return;
      if (req.method !== 'GET' || !PAGE.test(url.pathname)) return sendProblem(res, 404, 'not found');
      try {
        await route(url, res);
      } catch (err) {
        if (err instanceof BadRequest || err instanceof QueryError) return sendProblem(res, 400, err.message);
        sendProblem(res, 503, `the catalog is unavailable: ${err instanceof Error ? err.message : String(err)}`);
      }
    },
    stop: () => comments.close(),
  };
}

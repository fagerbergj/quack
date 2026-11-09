// The contracts extension's own read: each Contract Studio session's contract, from its sealed artifacts in the catalog.
import type { IncomingMessage, ServerResponse } from 'node:http';
import { escapeTerm, sendProblem, serviceHttp, type App, type AppContext } from '@quack/sdk';
import { APPROVED_KIND, CHANGES_KIND } from './approval.ts';
import { CONTRACT_KIND } from './spec.ts';

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
const parse = (content: string | undefined): Record<string, unknown> => { try { return JSON.parse(content ?? '') ?? {}; } catch { return {}; } };
const newest = (all: Artifact[], kind: string) => all.filter((a) => a.kind === kind).sort((a, b) => a.version - b.version).at(-1);
const versionOf = (a: Artifact | undefined) => (typeof parse(a?.content).version === 'number' ? (parse(a?.content).version as number) : undefined);

/** A contract from its session's sealed artifacts and its propose_contract asks; the core row carries none of it. */
export function contractOf(key: string, fallbackTitle: string | undefined, artifacts: Artifact[], asks: Ask[]): ContractRecord {
  const spec = parse(newest(artifacts, CONTRACT_KIND)?.content);
  const proposals = asks.filter((a) => a.toolCall?.tool === 'propose_contract');
  const proposed = Math.max(...proposals.map((a) => Number((a.toolCall?.args as { version?: unknown } | null)?.version) || 0));
  return {
    key, title: typeof spec.title === 'string' ? spec.title : (fallbackTitle ?? 'New contract'), kind: typeof spec.kind === 'string' ? spec.kind : 'rest',
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

/** The app: GET /contracts reads every live Contract Studio session, and GET /contracts/{key} one with its versions, through the catalog, never starting a process. */
export function contractsApp(ctx: AppContext): App {
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
  async function list(): Promise<ContractRecord[]> {
    const q = encodeURIComponent(`key:${escapeTerm('contracts:')}* AND -state:archived`);
    const sessions = await all<{ key: string; title?: string }>(`/sessions?q=${q}`, 'sessions');
    return Promise.all(sessions.map(async (s) => {
      const { artifacts, asks } = await records(s.key);
      return contractOf(s.key, s.title, artifacts, asks);
    }));
  }
  // One contract with every spec version's status; undefined when the catalog has no such session.
  async function one(key: string): Promise<(ContractRecord & { versions: SpecVersion[] }) | undefined> {
    const row = await ctx.fetch(`${ctx.distributorUrl}/sessions/${encodeURIComponent(key)}`);
    if (row.status === 404) return undefined;
    if (!row.ok) throw new Error(`/sessions/${key} answered ${row.status}`);
    const { title } = (await row.json()) as { title?: string };
    const { artifacts, asks } = await records(key);
    return { ...contractOf(key, title, artifacts, asks), versions: versionsOf(artifacts, asks) };
  }
  async function route(path: string, res: ServerResponse) {
    if (path === '/contracts') return send(res, 200, { contracts: await list() });
    const key = keyOf(path);
    const found = key.startsWith('contracts:') ? await one(key) : undefined;
    return found ? send(res, 200, found) : sendProblem(res, 404, 'no such contract');
  }
  return {
    async handle(req: IncomingMessage, res: ServerResponse) {
      const path = new URL(req.url ?? '', 'http://x').pathname;
      if (req.method !== 'GET' || !PAGE.test(path)) return sendProblem(res, 404, 'not found');
      try {
        await route(path, res);
      } catch (err) {
        sendProblem(res, 503, `the catalog is unavailable: ${err instanceof Error ? err.message : String(err)}`);
      }
    },
  };
}

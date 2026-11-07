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

type Artifact = { kind: string; version: number; content?: string };
type Ask = { state: string; toolCall?: { tool: string; args: unknown } };

const { send } = serviceHttp();
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

/** The app: GET /contracts reads every live Contract Studio session through the catalog, never starting a process. */
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
  async function list(): Promise<ContractRecord[]> {
    const q = encodeURIComponent(`key:${escapeTerm('contracts:')}* AND -state:archived`);
    const sessions = await all<{ key: string; title?: string }>(`/sessions?q=${q}`, 'sessions');
    return Promise.all(sessions.map(async (s) => {
      const base = `/sessions/${encodeURIComponent(s.key)}`;
      const [artifacts, asks] = await Promise.all([all<Artifact>(`${base}/artifacts?q=${encodeURIComponent('kind:contract.*')}`, 'artifacts'), all<Ask>(`${base}/asks?q=${encodeURIComponent('tool:propose_contract')}`, 'asks')]);
      return contractOf(s.key, s.title, artifacts, asks);
    }));
  }
  return {
    async handle(req: IncomingMessage, res: ServerResponse) {
      if (req.method !== 'GET' || new URL(req.url ?? '', 'http://x').pathname !== '/contracts') return sendProblem(res, 404, 'not found');
      try {
        send(res, 200, { contracts: await list() });
      } catch (err) {
        sendProblem(res, 503, `the catalog is unavailable: ${err instanceof Error ? err.message : String(err)}`);
      }
    },
  };
}

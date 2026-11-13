import type { contentText } from '@earendil-works/pi-ai';
import { escapeTerm } from '@quack/sdk';

/** A Sessions API v9 row, as far as the home tools read it. */
export type SessionRow = {
  key: string; agent: string; title?: string; state: 'running' | 'idle' | 'stopped' | 'archived'; createdAt: string; lastActivity: string; pendingAsks: number;
  lastRun?: { outcome: 'succeeded' | 'denied' | 'failed' | 'aborted'; endedAt: string; failedSteps: Array<{ step: string; error: string }> };
};
/** The distributor's export of a session: its transcript entries (pi-durable models) and every sealed version. */
export type SessionExport = { transcript: Array<{ kind: string; model: Array<{ content?: Parameters<typeof contentText>[0] }> }>; artifacts: Array<{ kind: string; version: number; content: string; sealedAt: number }> };
type Ask = { id: string; type: 'approval' | 'question'; toolCall?: { tool: string }; prompt?: { question: string; options: string[] } };

export type Distributor = ReturnType<typeof distributor>;

/** The distributor's HTTP, for the home tools. Callers check a key with `session()` first: process routes of a key it does not know answer 404. */
export function distributor(url: string, doFetch: typeof fetch = fetch) {
  const path = (key: string, rest: string) => `${url}/sessions/${encodeURIComponent(key)}${rest}`;
  async function call<T>(target: string, init?: RequestInit): Promise<T> {
    const res = await doFetch(target, init);
    const body = (await res.json().catch(() => null)) as (T & { detail?: string; error?: string }) | null;
    if (!res.ok) throw Object.assign(new Error(body?.detail ?? body?.error ?? `HTTP ${res.status}`), { status: res.status });
    return body as T;
  }
  const post = (target: string, body?: unknown) => call<unknown>(target, { method: 'POST', ...(body !== undefined && { body: JSON.stringify(body) }) });
  const list = (what: string, q: string) => `${url}/${what}?${new URLSearchParams({ q, pageSize: '1000' })}`;
  /** Every session matching the Lucene `q`; pages are followed to the end. */
  async function sessions(q?: string): Promise<SessionRow[]> {
    const rows: SessionRow[] = [];
    for (let token = ''; ;) {
      const page = await call<{ sessions: SessionRow[]; nextPageToken?: string }>(`${url}/sessions?${new URLSearchParams({ pageSize: '1000', ...(q && { q }), ...(token && { pageToken: token }) })}`);
      rows.push(...page.sessions);
      if (!page.nextPageToken) return rows;
      token = page.nextPageToken;
    }
  }
  return {
    sessions,
    /** One session, or undefined when the distributor knows no such key. */
    session: async (key: string): Promise<SessionRow | undefined> => (await sessions(`key:${escapeTerm(key)}`))[0],
    // A session credential's single-key reads reach only its own key, so asks come from the cross-session listing.
    pendingAsks: async (key: string) => (await call<{ asks: Ask[] }>(list('asks', `session:${escapeTerm(key)} AND state:pending`))).asks,
    // The export reads a stopped session's store without booting it, and is the one route to another session's artifact content.
    exported: (key: string) => call<SessionExport>(path(key, '/export')),
    send: (key: string, agent: string, input: string, requestId: string) => post(`${path(key, '/invocations')}?agent=${encodeURIComponent(agent)}`, { input, requestId, whenBusy: 'followUp' }),
    abort: (key: string) => post(path(key, '/abort')),
    resume: (key: string) => post(path(key, '/resume')),
    /** Records a key Home started with Home's app, which tells home:main when its runs end. */
    started: (key: string) => post(`${url}/apps/home/started`, { key }),
  };
}

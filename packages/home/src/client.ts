/** A Sessions API v9 row, as far as the home tools read it. */
export type SessionRow = {
  key: string; agent: string; title?: string; state: 'running' | 'idle' | 'stopped' | 'archived'; createdAt: string; lastActivity: string; pendingAsks: number;
  lastRun?: { outcome: 'succeeded' | 'denied' | 'failed' | 'aborted'; endedAt: string; failedSteps: Array<{ step: string; error: string }> };
};
type Ask = { id: string; type: 'approval' | 'question'; toolCall?: { tool: string }; prompt?: { question: string; options: string[] } };

export type Distributor = ReturnType<typeof distributor>;

/** The distributor's HTTP, for the home tools. Callers check a key with `session()` first: reads of a key it does not know answer 404. */
export function distributor(url: string, doFetch: typeof fetch = fetch) {
  const path = (key: string, rest: string) => `${url}/sessions/${encodeURIComponent(key)}${rest}`;
  async function call<T>(target: string, init?: RequestInit): Promise<T> {
    const res = await doFetch(target, init);
    const body = (await res.json().catch(() => null)) as (T & { detail?: string; error?: string }) | null;
    if (!res.ok) throw Object.assign(new Error(body?.detail ?? body?.error ?? `HTTP ${res.status}`), { status: res.status });
    return body as T;
  }
  const post = (target: string, body?: unknown) => call<unknown>(target, { method: 'POST', ...(body !== undefined && { body: JSON.stringify(body) }) });
  return {
    /** Every session matching the Lucene `q`; pages are followed to the end. */
    async sessions(q?: string): Promise<SessionRow[]> {
      const rows: SessionRow[] = [];
      for (let token = ''; ;) {
        const page = await call<{ sessions: SessionRow[]; nextPageToken?: string }>(`${url}/sessions?${new URLSearchParams({ pageSize: '1000', ...(q && { q }), ...(token && { pageToken: token }) })}`);
        rows.push(...page.sessions);
        if (!page.nextPageToken) return rows;
        token = page.nextPageToken;
      }
    },
    /** One session, or undefined when the distributor knows no such key. */
    session: (key: string) => call<SessionRow>(path(key, '')).catch((err: Error & { status?: number }) => (err.status === 404 ? undefined : Promise.reject(err))),
    // Catalog reads: none of them starts the session's process.
    artifacts: async (key: string) => (await call<{ artifacts: Array<{ kind: string; version: number; content: string; summary?: string; deliveries?: Array<{ where: string; link: string }> }> }>(path(key, '/artifacts?pageSize=1000'))).artifacts,
    pendingAsks: async (key: string) => (await call<{ asks: Ask[] }>(path(key, '/asks?q=state:pending&pageSize=1000'))).asks,
    messages: async (key: string, limit: number) => (await call<{ messages: Array<{ role: string; text: string }> }>(path(key, `/messages?limit=${limit}`))).messages,
    send: (key: string, agent: string, input: string, requestId: string) => post(`${path(key, '/invocations')}?agent=${encodeURIComponent(agent)}`, { input, requestId, whenBusy: 'followUp' }),
    abort: (key: string) => post(path(key, '/abort')),
    resume: (key: string) => post(path(key, '/resume')),
  };
}

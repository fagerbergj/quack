export type SessionRow = { key: string; agent: string; title?: string; running: boolean; active?: boolean; note?: string; createdAt: string; lastActivity: string };
export type Standing = { needsYou: Set<string>; failed: Set<string> };

export type Distributor = ReturnType<typeof distributor>;

/** The distributor's HTTP, for the home tools. Callers check a key against `sessions()` first: reads of a key it does not know answer 404. */
export function distributor(url: string, doFetch: typeof fetch = fetch) {
  const path = (key: string, rest: string) => `${url}/sessions/${encodeURIComponent(key)}${rest}`;
  async function call<T>(target: string, init?: RequestInit): Promise<T> {
    const res = await doFetch(target, init);
    const body = (await res.json().catch(() => null)) as (T & { error?: string }) | null;
    if (!res.ok) throw new Error(body?.error ?? `HTTP ${res.status}`);
    return body as T;
  }
  const post = (target: string, body?: unknown) => call<unknown>(target, { method: 'POST', ...(body !== undefined && { body: JSON.stringify(body) }) });
  // The index answers 404 when it is off; the lists then say nothing about who needs you.
  const indexed = async (rest: string, field: string): Promise<Array<{ session: string }>> => {
    const rows: Array<{ session: string }> = [];
    for (let token = ''; ;) {
      const res = await doFetch(`${url}/index/${rest}${token && `${rest.includes('?') ? '&' : '?'}pageToken=${token}`}`).catch(() => undefined);
      if (!res?.ok) return [];
      const page = (await res.json()) as Record<string, Array<{ session: string }>> & { nextPageToken?: string };
      rows.push(...(page[field] ?? []));
      if (!page.nextPageToken) return rows;
      token = page.nextPageToken;
    }
  };
  return {
    sessions: async () => (await call<{ sessions: SessionRow[] }>(`${url}/sessions`)).sessions,
    async standing(): Promise<Standing> {
      const [questions, approvals, failures] = await Promise.all([indexed('questions?state=pending', 'questions'), indexed('approvals?state=pending', 'approvals'), indexed('failures', 'failures')]);
      return { needsYou: new Set([...questions, ...approvals].map((r) => r.session)), failed: new Set(failures.map((r) => r.session)) };
    },
    ping: (key: string) => call<{ status: string; waitingOnHuman?: boolean; note?: string }>(path(key, '/ping')),
    artifacts: async (key: string) => (await call<{ artifacts: Array<{ kind: string; version: number; content: string; summary?: string; verdict: { passed: boolean } | null; links?: Array<{ label: string; url: string }> }> }>(path(key, '/artifacts'))).artifacts,
    questions: async (key: string) => (await call<{ questions: Array<{ question: string; options: string[] }> }>(path(key, '/questions'))).questions,
    failures: async (key: string) => (await call<{ failures: Array<{ step: string; error: string }> }>(path(key, '/failures'))).failures,
    messages: async (key: string, limit: number) => (await call<{ messages: Array<{ role: string; text: string }> }>(path(key, `/messages?limit=${limit}`))).messages,
    send: (key: string, agent: string, input: string, requestId: string) => post(`${path(key, '/invocations')}?agent=${encodeURIComponent(agent)}`, { input, requestId, whenBusy: 'followUp' }),
    abort: (key: string) => post(path(key, '/abort')),
    resume: (key: string) => post(path(key, '/resume')),
  };
}

import { ReuseDoc } from '../src/reuse.ts';

export type Clef = (row: string) => number | 'error';
export type Judge = (row: string) => string | 'error';

// One session's api as the judge sees it across rounds: the reuse store survives between calls, while each round
// uses a memo of its own, as a new tool call does. The decision model answers a lone row by `clef`; the judge model by `judge`.
export function sessionApi(clef: Clef, judge: Judge) {
  const store = { answers: {} as Record<string, { met: boolean; resolver: string; note?: string; version: number }> };
  const memos = new Map<string, unknown>();
  const seen = { clef: [] as string[], judge: [] as string[], commits: 0 };
  const rowsOf = (text: string) => [...text.matchAll(/"id": "(\w+)"/g)].map((m) => m[1]);
  const api = {
    conversationId: 'c',
    now: () => 0,
    async memo(name: string, ...rest: unknown[]) {
      if (rest.length === 2 && !memos.has(name)) memos.set(name, rest[0]);
      return memos.get(name);
    },
    snapshot: async (doc: unknown) => (doc === ReuseDoc ? structuredClone(store) : undefined),
    commit: async (change: (tx: unknown) => Promise<unknown>) => (seen.commits++, change({ doc: async () => store })),
    agent: async () => ({}),
    models: {
      getAllModels: () => [{ id: 'clef', type: 'classifier' }],
      classify: async (_m: unknown, req: { state: { row?: { id: string }; items?: Array<{ row: { id: string } }> }; questions: Record<string, unknown> }) => {
        const rows = req.state.items ? req.state.items.map((it) => it.row.id) : [req.state.row!.id];
        seen.clef.push(...rows);
        if (rows.some((r) => clef(r) === 'error')) return { stopReason: 'error', answers: {} };
        return { stopReason: 'stop', answers: Object.fromEntries(Object.keys(req.questions).map((q, n) => [q, { type: 'bool', probability: clef(rows[n] ?? rows[0]) }])) };
      },
      getModel: (provider: string, id: string) => ({ provider, id }),
      completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
        const prompt = r.messages[0].content;
        const rows = prompt.startsWith('Evidence:') ? [...prompt.matchAll(/^- q\d+: About items\[\d+\] \(rows (\w+)\)/gm)].map((m) => m[1]) : rowsOf(prompt);
        seen.judge.push(...rows);
        const replies = rows.map((row) => judge(row));
        if (replies.includes('error')) return { stopReason: 'error', content: [] };
        const text = prompt.startsWith('Evidence:') ? `{${replies.map((t, n) => (t.startsWith('{') ? `"q${n}": ${t}` : '')).filter(Boolean).join(', ')}}` : replies[0];
        return { stopReason: 'stop', content: [{ type: 'text', text }] };
      },
    },
  };
  return { api: api as never, store, seen };
}

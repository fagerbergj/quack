import type { FakeScript } from '../fake-model.ts';
import { visible, type Scenario } from '../scenario.ts';

const HOME = 'home:main';
const NOTICE = 'A session you started ended';
const report = '## Summary\nExample.com is a reserved example domain.\n## Assumptions\nNone.\n## Findings\n- Example.com is reserved for use in documents. [s1]\n';
const text = (m: { content?: unknown }) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? ''));

// Home starts research on the person's ask and acknowledges the notice; the researcher fetches, writes and submits a report; a request without tools is the judge.
const script: FakeScript = ({ messages = [], tools = [] }) => {
  const names = tools.map((t) => t.function?.name);
  const last = messages.at(-1);
  if (!names.length) return { content: JSON.stringify({ choice: 'yes', confidence: 1, reason: 'qa' }) };
  if (names.includes('start')) {
    if (last?.role === 'tool') return { content: 'Started; I will tell you when it finishes.' };
    if (text(last ?? {}).includes(NOTICE)) return { content: `Heard: ${text(last!).split('\n')[0]}` };
    return { tool: { name: 'start', arguments: { action: 'research_question', params: { question: 'What is example.com for?' } } } };
  }
  const result = last?.role === 'tool' ? text(last) : '';
  if (!result) return { tool: { name: 'web_fetch', arguments: { urls: ['https://example.com/'] } } };
  if (/passed\.|Stop: this run is over|revision limit is reached/.test(result)) return { content: 'Done.' };
  if (/\[s1\]|failed/.test(result)) return { tool: { name: 'write', arguments: { path: 'artifacts/report.md', content: report } } };
  return { tool: { name: 'submit_report', arguments: {} } };
};

const json = async <T>(url: string, init?: RequestInit): Promise<T> => {
  const res = await fetch(url, init);
  if (!res.ok) throw new Error(`${init?.method ?? 'GET'} ${url}: ${res.status} ${await res.text()}`);
  return (await res.json()) as T;
};

// QA polls the live stack; the bound only matters when the code is broken.
async function until<T>(what: string, probe: () => Promise<T | undefined>): Promise<T> {
  for (let i = 0; i < 240; i++) {
    const got = await probe();
    if (got !== undefined) return got;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`timed out waiting for ${what}`);
}

type Row = { key: string; state: string; lastRun?: { outcome: string } };

const scenario: Scenario = {
  name: 'home-hears-runs',
  stack: { script, apps: ['home'] },
  async run(ctx) {
    const home = `${ctx.url}/sessions/${encodeURIComponent(HOME)}`;
    await ctx.step('Home starts a research session once the person approves', async () => {
      await json(`${home}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: 'research what example.com is for' }) });
      const ask = await until('the start approval', async () => (await json<{ asks: Array<{ id: string }> }>(`${home}/asks?q=state:pending`)).asks[0]);
      await json(`${home}/asks/${encodeURIComponent(ask.id)}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ resolution: { choice: 'approve' } }) });
    });
    await ctx.step('the research session runs to its end', async () => {
      const row = await until('the research run to end', async () => {
        const [r] = (await json<{ sessions: Row[] }>(`${ctx.url}/sessions?q=${encodeURIComponent('key:research\\:*')}`)).sessions;
        return r?.lastRun && r.state !== 'running' ? r : undefined;
      });
      const { artifacts } = await json<{ artifacts: Array<{ kind: string; version: number; summary?: string }> }>(`${ctx.url}/sessions/${encodeURIComponent(row.key)}/artifacts`);
      console.log(`  ${row.key}: ${row.lastRun!.outcome}, sealed ${JSON.stringify(artifacts.map((a) => [a.kind, a.version, a.summary]))}`);
      if (!artifacts.length) throw new Error(`the researcher sealed no report: ${JSON.stringify(await json(`${ctx.url}/sessions/${encodeURIComponent(row.key)}/export`)).slice(0, 4000)}`);
    });
    await ctx.step("home's conversation receives one notice, and the agent answers it", async () => {
      const messages = await until('the notice and its answer', async () => {
        const m = (await json<{ messages: Array<{ role: string; text: string }> }>(`${home}/messages?limit=20`)).messages;
        return m.at(-1)?.role === 'assistant' && m.at(-1)!.text.startsWith('Heard:') ? m : undefined;
      });
      const notices = messages.filter((m) => m.role === 'user' && m.text.includes(NOTICE));
      if (notices.length !== 1 || !/Newest result: research\.report v\d/.test(notices[0].text) || !/#\/s\/research%3A/.test(notices[0].text)) throw new Error(JSON.stringify(messages));
      console.log(`  notice: ${JSON.stringify(notices[0].text)}`);
    });
    await ctx.step('the notice shows in the home conversation', async () => {
      const page = await ctx.browser.newPage({ viewport: { width: 1280, height: 900 } });
      try {
        await page.goto(`${ctx.url}/#/s/${encodeURIComponent(HOME)}`);
        await visible(page.getByRole('log', { name: 'Transcript' }).getByText(NOTICE).first());
        await ctx.shot(page, 'home-hears-runs-1280');
      } finally {
        await page.close();
      }
    });
  },
};
export default scenario;

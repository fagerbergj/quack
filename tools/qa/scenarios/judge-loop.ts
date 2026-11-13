import type { FakeScript } from '../fake-model.ts';
import { paneOf, visible, type Scenario } from '../scenario.ts';

// The research agent through the judge extension, with the fake model as both writer and judge (no judgeModel: the agent's own).
const SOURCE = 'https://example.com/';
const report = (cites: string) => `## Summary\nExample.com is a reserved example domain.\n## Assumptions\nNone.\n## Findings\n- Example.com is reserved for examples in documents. ${cites}\n`;
const text = (m: { content?: unknown }) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? ''));

// The writer's next move from the flow its input names and the last tool result; a request without tools is the judge asking.
const script: FakeScript = ({ messages = [], tools = [] }) => {
  const all = JSON.stringify(messages);
  if (!tools.length) return { content: JSON.stringify({ choice: /contradict it\?|conflict with another passage/.test(all) ? 'no' : 'yes', confidence: 1, reason: 'qa' }) };
  const flow = text(messages.find((m) => m.role === 'user') ?? {});
  const last = messages.at(-1)?.role === 'tool' ? text(messages.at(-1)!) : '';
  const written = messages.filter((m) => m.role === 'tool' && /passed|failed/.test(text(m))).length;
  if (!last) return { tool: { name: 'web_fetch', arguments: { urls: [SOURCE] } } };
  if (/passed\.|Stop: this run is over/.test(last) || (/revision limit is reached/.test(last) && !flow.includes('flow:past'))) return { content: 'Done.' };
  if (/\[s1\]/.test(last) || /failed|limit is reached/.test(last)) return { tool: { name: 'write', arguments: { path: 'artifacts/report.md', content: report(written ? '[s1]' : '[s1][s2]') } } };
  return { tool: { name: 'submit_report', arguments: {} } };
};

const agent = (maxRevisions: number) => ({
  definition: { name: `qa-judge-${maxRevisions}`, version: 1, model: 'local/qa-fake', prompt: 'prompt.md', extensions: { judge: { maxRevisions }, coding: {}, research: {} } },
  prompt: 'You research and write artifacts/report.md, then call submit_report.\n',
});

type Export = { transcript: unknown[]; artifacts: Array<{ kind: string; version: number; verdict: { passed: boolean; limitReached: boolean } | null }> };

async function runFlow(url: string, key: string, agentName: string, flow: string): Promise<{ results: string; versions: Export['artifacts'] }> {
  const res = await fetch(`${url}/sessions/${key}/invocations?agent=${agentName}`, { method: 'POST', body: JSON.stringify({ input: `flow:${flow} What is example.com for?`, wait: true }) });
  if (!res.ok) throw new Error(`run ${key}: ${res.status} ${await res.text()}`);
  const exported = (await (await fetch(`${url}/sessions/${key}/export`)).json()) as Export;
  const results = JSON.stringify(exported.transcript);
  const versions = exported.artifacts.filter((a) => a.kind === 'research.report');
  console.log(`  ${key}: ${JSON.stringify(versions.map((v) => [v.version, v.verdict && { passed: v.verdict.passed, limitReached: v.verdict.limitReached }]))}`);
  return { results, versions };
}

const expect = (ok: boolean, what: string, got: unknown) => {
  if (!ok) throw new Error(`${what}: ${JSON.stringify(got).slice(0, 2000)}`);
};

const scenario: Scenario = {
  name: 'judge-loop',
  stack: { script, agents: { 'qa-judge-2': agent(2), 'qa-judge-0': agent(0) } },
  async run(ctx) {
    await ctx.step('revise then pass: a failing version comes back, the revision passes', async () => {
      const { results, versions } = await runFlow(ctx.url, 'qa-judge-revise', 'qa-judge-2', 'revise');
      expect(JSON.stringify(versions.map((v) => v.verdict?.passed)) === '[false,true]', 'verdicts', versions);
      expect(/cites s2, which this session never fetched/.test(results) && /Revise the file to address each point, then call submit_report again/.test(results), 'the failure fed back', results);
      expect(/research\.report v2: \d+% \(research\.report@\d+\), passed\. Deliver version 2\./.test(results), 'the pass note', results);
    });
    await ctx.step('limit release: a failing version at the limit goes out with its failures marked', async () => {
      const { results, versions } = await runFlow(ctx.url, 'qa-judge-limit', 'qa-judge-0', 'limit');
      expect(versions.length === 1 && versions[0].verdict?.limitReached === true, 'released at the limit', versions);
      expect(/The revision limit is reached\. Deliver version 1 with these failures marked\./.test(results), 'the release note', results);
    });
    await ctx.step('past the limit: another seal is refused and ends the run', async () => {
      const { results, versions } = await runFlow(ctx.url, 'qa-judge-past', 'qa-judge-0', 'past');
      expect(versions.length === 2 && versions[1].verdict === null, 'v2 sealed, not judged', versions);
      expect(/research\.report was already checked 1 times in this run, the most it gets\. Stop: this run is over\./.test(results), 'the refusal', results);
    });
    await ctx.step('the passed report shows its verdict beside the chat', async () => {
      const page = await ctx.browser.newPage({ viewport: { width: 1280, height: 900 } });
      try {
        await page.goto(`${ctx.url}/#/s/qa-judge-revise`);
        await visible(paneOf(page, 'Report').getByText('Example.com is reserved for examples in documents.').first());
        await ctx.shot(page, 'judge-loop-revise-1280');
      } finally {
        await page.close();
      }
    });
  },
};
export default scenario;

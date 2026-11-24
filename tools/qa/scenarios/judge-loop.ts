import type { FakeScript } from '../fake-model.ts';
import { eachView, paneOf, visible, type Scenario } from '../scenario.ts';

// The research agent through the judge extension, with the fake model as both writer and judge (no judgeModel: the agent's own).
const SOURCE = 'https://example.com/';
const GOOD = 'Example.com is reserved for examples in documents.';
const BAD = 'Example.com is run by the moon.';
// Six findings the judge answers slowly, so a step can watch a round's counts move.
const LIVE = Array.from({ length: 6 }, (_, i) => `Example.com serves example ${i + 1}.`);
const report = (...findings: string[]) => `## Summary\nExample.com is a reserved example domain.\n## Assumptions\nNone.\n## Findings\n${findings.map((f) => `- ${f} [s1]\n`).join('')}`;
const text = (m: { content?: unknown }) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? ''));

// supported_by_source finds BAD's claim silent in the source and GOOD's stated. The regex keys on the question wording in packages/research/src/report.ts.
const verdict = (question: string, evidence: string) => (/state the claim\?/.test(question) ? (evidence.includes(BAD) ? 'silent' : 'states') : 'yes');
const choice = (c: string) => ({ choice: c, confidence: 1, reason: 'qa' });

// The judge asks both source checks of a finding in one packed request ({"q0":..,"q1":..}); answer each question by its own text, and a single question by the whole request.
function judgeReply(all: string, prompt: string) {
  if (process.env.JL_DEBUG) console.log("JUDGE", prompt.slice(0, 150).replace(/\n/g, " "), "..", prompt.slice(-200).replace(/\n/g, " "));
  const slow = /serves example \d/.test(prompt) ? { delayMs: 1500 } : {};
  if (!/"q0": \{/.test(prompt)) return { content: JSON.stringify(choice(verdict(all, all))), ...slow };
  const qs = [...prompt.matchAll(/^- (q\d+): (.*)$/gm)];
  return { content: JSON.stringify(Object.fromEntries(qs.map(([, id, q]) => [id, choice(verdict(q, prompt))]))), ...slow };
}

// The writer's next move from the flow its input names and the last tool result; a request without tools is the judge asking.
const script: FakeScript = ({ messages = [], tools = [] }) => {
  const all = JSON.stringify(messages);
  if (!tools.length) return judgeReply(all, text(messages.at(-1) ?? {}));
  const flow = text(messages.find((m) => m.role === 'user') ?? {});
  const last = messages.at(-1)?.role === 'tool' ? text(messages.at(-1)!) : '';
  const written = messages.filter((m) => m.role === 'tool' && /passed|failed/.test(text(m))).length;
  if (!last) return { tool: { name: 'web_fetch', arguments: { urls: [SOURCE] } } };
  if (/passed\.|Stop: this run is over/.test(last) || (/revision limit is reached/.test(last) && !flow.includes('flow:past'))) return { content: 'Done.' };
  if (/\[s1\]/.test(last) || /failed|limit is reached/.test(last)) return { tool: { name: 'write', arguments: { path: 'artifacts/report.md', content: flow.includes('flow:live') ? report(...LIVE) : report(written ? GOOD : BAD) } } };
  return { tool: { name: 'submit_report', arguments: {} } };
};

const agent = (maxRevisions: number) => ({
  definition: { name: `qa-judge-${maxRevisions}`, version: 1, model: 'local/qa-fake', prompt: 'prompt.md', extensions: { judge: { maxRevisions }, coding: {}, research: {} } },
  prompt: 'You research and write artifacts/report.md, then call submit_report.\n',
});

type Export = { transcript: unknown[]; artifacts: Array<{ kind: string; version: number; verdict: { passed: boolean; limitReached: boolean; checks?: Array<{ id: string; label: string; met: number | null }> } | null }> };

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
      const check = versions[0].verdict?.checks?.find((c) => c.id === 'supported_by_source');
      expect(check?.met === 0 && check.label === "A claim isn't stated by any source it cites", 'supported_by_source judged the claim (a dropped claim unit would pass the draft)', versions[0]);
      expect(/missed claims_grounded\.supported_by_source/.test(results) && /Revise the file to address each point, then call submit_report again/.test(results), 'the failure fed back', results);
      expect(/research\.report v2: \d+% \(research\.report@\d+\), passed\. Version 2 is final for this run; finish without revising it\./.test(results), 'the pass note', results);
    });
    await ctx.step('limit release: a failing version at the limit goes out with its failures marked', async () => {
      const { results, versions } = await runFlow(ctx.url, 'qa-judge-limit', 'qa-judge-0', 'limit');
      expect(versions.length === 1 && versions[0].verdict?.limitReached === true, 'released at the limit', versions);
      expect(/The revision limit is reached\. Deliver version 1 with these failures marked\./.test(results), 'the release note', results);
    });
    await ctx.step('past the limit: the released version is final; a change is refused, and a second one ends the run', async () => {
      const { results, versions } = await runFlow(ctx.url, 'qa-judge-past', 'qa-judge-0', 'past');
      expect(versions.length === 1 && versions[0].verdict?.limitReached === true, 'v1 stays the only version', versions);
      expect(/research\.report v1 is delivered, so it is final for this run and this change to artifacts\/report\.md was not sealed\./.test(results), 'the first refusal', results);
      expect(/research\.report v1 is delivered\. A second change after it was not sealed either, so this run is over\./.test(results), 'the second refusal', results);
    });
    await eachView(ctx, 'the released report opens on its failing check, marked, counted, and its claim read from the report', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/qa-judge-limit`);
      if (label.startsWith('400')) await page.getByRole('tab', { name: /^Report/ }).click();
      else await page.getByRole('button', { name: 'Show Report' }).click();
      const pane = paneOf(page, 'Report');
      await visible(pane.getByRole('img', { name: 'Failed' }).first());
      await visible(pane.getByText(`“${BAD}”`).first());
      await ctx.shot(page, `judge-loop-limit-${label}`);
    });
    await eachView(ctx, "a running double-check counts its items done, over a bar of who settled them", async (page, label) => {
      const key = `qa-judge-live-${label}`;
      const run = runFlow(ctx.url, key, 'qa-judge-0', 'live');
      try {
        // The judge starts once the report is sealed, within 10 s; on a phone the chat holds the running checks.
        const sealed = async () => ((await (await fetch(`${ctx.url}/sessions/${key}/artifacts`)).json().catch(() => ({}))) as { artifacts?: unknown[] }).artifacts?.length;
        for (let i = 0; i < 50 && !(await sealed()); i++) await new Promise((r) => setTimeout(r, 200));
        await page.goto(`${ctx.url}/#/s/${key}`);
        if (label.startsWith('400')) await page.getByRole('tab', { name: /^Chat/ }).click();
        await visible(page.getByRole('list', { name: 'Checks running' }).getByText('2/6'));
        await ctx.shot(page, `judge-loop-live-${label}`);
      } finally {
        await run;
      }
    });
    await ctx.step('the passed report shows its verdict beside the chat once expanded', async () => {
      const page = await ctx.browser.newPage({ viewport: { width: 1280, height: 900 } });
      try {
        await page.goto(`${ctx.url}/#/s/qa-judge-revise`);
        await page.getByRole('button', { name: 'Show Report' }).click();
        await visible(paneOf(page, 'Report').getByText('Example.com is reserved for examples in documents.').first());
        await ctx.shot(page, 'judge-loop-revise-1280');
      } finally {
        await page.close();
      }
    });
  },
};
export default scenario;

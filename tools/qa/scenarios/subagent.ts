import type { FakeScript } from '../fake-model.ts';
import { eachView, visible, type Scenario } from '../scenario.ts';

const KEY = 'qa-subagent';
const TASK = 'Summarize the retry backoff change';
const CHILD = 'Child answer: the backoff caps at 30 seconds.';

// The parent offers `subagent` and calls it once; the child, which has no such tool, answers; the parent then merges.
const script: FakeScript = ({ messages = [], tools = [] }) => {
  if (messages.at(-1)?.role === 'tool') return { content: `Merged: ${CHILD}` };
  if (tools.some((t) => t.function?.name === 'subagent')) return { tool: { name: 'subagent', arguments: { task: TASK } } };
  return { content: CHILD };
};

const agent = { name: 'qa-subagent', version: 1, model: 'local/qa-fake', prompt: 'prompt.md', extensions: { subagent: {} } };

const scenario: Scenario = {
  name: 'subagent',
  stack: { script, agents: { 'qa-subagent': { definition: agent, prompt: 'You are a QA agent. Hand tasks to a subagent.\n' } } },
  async run(ctx) {
    let child = '';
    await ctx.step('a run with one subagent call', async () => {
      const res = await fetch(`${ctx.url}/sessions/${KEY}/invocations?agent=qa-subagent`, { method: 'POST', body: JSON.stringify({ input: 'Delegate the summary', wait: true }) });
      const body = await res.text();
      if (!res.ok || !body.includes('Merged:')) throw new Error(`run failed: ${res.status} ${body}`);
    });
    await ctx.step('GET /messages?conversation= serves the child', async () => {
      const { transcript } = await (await fetch(`${ctx.url}/sessions/${KEY}/export`)).json() as { transcript: Array<{ kind: string; model: Array<{ details?: { conversationId?: string } }> }> };
      child = transcript.find((e) => e.kind === 'pi.tool-result')?.model[0]?.details?.conversationId ?? '';
      const res = await fetch(`${ctx.url}/sessions/${KEY}/messages?conversation=${encodeURIComponent(child)}&limit=50`);
      const { messages } = await res.json() as { messages: Array<{ role: string; text: string }> };
      const want = JSON.stringify([{ role: 'user', text: TASK }, { role: 'assistant', text: CHILD }]);
      if (!child || JSON.stringify(messages) !== want) throw new Error(`child ${child}: ${res.status} ${JSON.stringify(messages)}`);
      console.log(`  child conversation ${child}: ${JSON.stringify(messages)}`);
    });
    await eachView(ctx, 'subagent row nests the child', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/${KEY}`);
      const log = page.getByRole('log', { name: 'Transcript' });
      await visible(log.getByText(`Merged: ${CHILD}`));
      await log.getByText(/Worked/).first().click();
      // The activity line also names the tool, so the call row is the summary whose own label is exactly `subagent`.
      await log.locator('summary').filter({ has: page.getByText('subagent', { exact: true }) }).click();
      await log.getByText('Subagent transcript').click();
      // Scoped to the nested panel (the innermost match): the call's own result repeats the child's answer.
      const nested = log.locator('details').filter({ has: page.locator('summary', { hasText: 'Subagent transcript' }) }).last();
      await visible(nested.getByText(TASK));
      await visible(nested.getByText(CHILD));
      await ctx.shot(page, `subagent-${label}`);
    });
  },
};
export default scenario;

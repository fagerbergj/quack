import type { FakeScript } from '../fake-model.ts';
import type { Scenario } from '../scenario.ts';

const RESEARCH = 'research:qa-sky';
const SEEDED = 'research:qa-langfuse';
const ANSWER = 'The sky is blue because air scatters short wavelengths most.';

// The home agent calls read_session on the key its input names and answers with the tool's result. The researcher only
// answers, never submitting a report, so its run ends failed at the turn ceiling; the seeded session carries a sealed one.
const script: FakeScript = ({ messages = [], tools = [] }) => {
  const last = messages.at(-1);
  if (!tools.some((t) => t.function?.name === 'read_session')) return { content: ANSWER };
  if (last?.role === 'tool') return { content: `Read: ${String(last.content)}` };
  return { tool: { name: 'read_session', arguments: { key: String(last?.content).split(' ').at(-1) } } };
};

const invoke = async (url: string, key: string, agent: string, input: string) => {
  const res = await fetch(`${url}/sessions/${encodeURIComponent(key)}/invocations?agent=${agent}`, { method: 'POST', body: JSON.stringify({ input, wait: true }) });
  const body = await res.text();
  if (!res.ok) throw new Error(`${key}: ${res.status} ${body}`);
  return body;
};

const scenario: Scenario = {
  name: 'home-read-session',
  stack: { script, seed: 'artifacts' },
  async run(ctx) {
    await ctx.step('a researcher session runs on the fake model', async () => void (await invoke(ctx.url, RESEARCH, 'researcher', 'Why is the sky blue?')));
    for (const [key, want] of [[RESEARCH, ANSWER], [SEEDED, 'research.report v1']] as const) {
      await ctx.step(`the home agent reads ${key} through its session credential`, async () => {
        const body = await invoke(ctx.url, 'home:qa', 'quack', `read ${key}`);
        if (!body.includes(want) || body.includes('no session with key')) throw new Error(body);
        console.log(`  ${body.slice(0, 600)}`);
      });
    }
  },
};
export default scenario;

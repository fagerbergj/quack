import type { FakeScript } from '../fake-model.ts';
import type { Scenario } from '../scenario.ts';

const RESEARCH = 'research:qa-sky';
const SEEDED = 'research:qa-langfuse';
const ANSWER = 'The sky is blue because air scatters short wavelengths most.';

// The home agent calls read_session on the key its input names (with messages when it says so) and answers with the result. The researcher only
// answers, never submitting a report, so its run ends failed at the turn ceiling; the seeded session carries a sealed one.
const script: FakeScript = ({ messages = [], tools = [] }) => {
  const last = messages.at(-1);
  if (!tools.some((t) => t.function?.name === 'read_session')) return { content: ANSWER };
  if (last?.role === 'tool') return { content: `Read: ${String(last.content)}` };
  const words = String(last?.content).split(' ');
  return { tool: { name: 'read_session', arguments: { key: words.at(-1), ...(words.includes('messages') && { messages: true }) } } };
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
    // The seeded report's text leads the read, and messages come only on request.
    const reads = [[SEEDED, `read ${SEEDED}`, (a: string) => a.startsWith('Read: --- research.report v1 ---\n## Short answer\n') && !a.includes('Last messages')],
      [RESEARCH, `read messages ${RESEARCH}`, (a: string) => a.includes(`Last messages:\n- assistant: ${ANSWER}`)]] as const;
    for (const [key, input, ok] of reads) {
      await ctx.step(`the home agent ${input} through its session credential`, async () => {
        const { answer } = JSON.parse(await invoke(ctx.url, 'home:qa', 'quack', input)) as { answer: string };
        if (!ok(answer)) throw new Error(answer);
        console.log(`  ${key}: ${JSON.stringify(answer.slice(0, 700))}`);
      });
    }
  },
};
export default scenario;

import type { FakeScript } from '../fake-model.ts';
import { answer, SDK_V1, SDK_V2, sdkSpec } from '../seed-contracts.ts';
import type { Scenario } from '../scenario.ts';

// A QA contracts agent with the judge on: the judge's gate passes each version, then the contracts gate puts it to the person and waits.
const KEY = 'contracts:qa-gate';
const text = (m: { content?: unknown }) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? ''));
const write = (version: 1 | 2) => ({ tool: { name: 'write', arguments: { path: 'artifacts/spec.json', content: JSON.stringify(sdkSpec(version, version === 1 ? SDK_V1 : SDK_V2), null, 2) } } });

// Judge questions start "Decision:" and get the passing answer; the writer drafts v1, revises to v2 on a rejection, and stops once approved.
const script: FakeScript = ({ messages = [] }) => {
  const last = messages.at(-1);
  if (last?.role === 'user' && text(last).startsWith('Decision:')) return { content: JSON.stringify({ choice: answer(text(last)), confidence: 1, reason: 'qa' }) };
  if (last?.role !== 'tool') return write(1);
  const result = text(last);
  if (/is approved\./.test(result)) return { content: 'Done.' };
  if (/Rejected by/.test(result)) return write(2);
  return { tool: { name: 'submit_contract', arguments: {} } };
};

const agent = { definition: { name: 'qa-contracts', version: 1, model: 'local/qa-fake', prompt: 'prompt.md', extensions: { judge: { maxRevisions: 2 }, coding: {}, contracts: {} } }, prompt: 'You write artifacts/spec.json and call submit_contract.\n' };

type Ask = { id: string; state: string; toolCall?: { args?: { version?: number } } };
const route = (url: string) => `${url}/sessions/${encodeURIComponent(KEY)}`;

// The pending approval for spec version `version`, once the contracts gate opens it; the fake model answers within a few seconds.
async function pendingFor(url: string, version: number): Promise<Ask> {
  for (let i = 0; i < 120; i++) {
    // Until the invocation creates the session, the route answers a problem rather than a list.
    const { asks = [] } = (await (await fetch(`${route(url)}/asks`)).json()) as { asks?: Ask[] };
    const found = asks.find((a) => a.state === 'pending' && a.toolCall?.args?.version === version);
    if (found) return found;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`no pending approval for version ${version}`);
}

const resolve = (url: string, id: string, choice: 'approve' | 'reject', reason: string) =>
  fetch(`${route(url)}/asks/${encodeURIComponent(id)}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ resolution: { choice, reason } }) });

const scenario: Scenario = {
  name: 'contracts-gate',
  stack: { script, agents: { 'qa-contracts': agent } },
  async run(ctx) {
    let done: Promise<Response> | undefined;
    await ctx.step('a passed version is put to the person, and a rejection goes back to the model as feedback', async () => {
      done = fetch(`${route(ctx.url)}/invocations?agent=qa-contracts`, { method: 'POST', body: JSON.stringify({ input: 'Define the extension SDK manifest types', wait: true }) });
      done.catch(() => undefined);
      const first = await pendingFor(ctx.url, 1);
      const res = await resolve(ctx.url, first.id, 'reject', 'Add an optional stop hook to App');
      if (!res.ok) throw new Error(`reject: ${res.status} ${await res.text()}`);
      console.log(`  v1 approval ${first.id} rejected`);
    });
    await ctx.step('the revision passes the judge, is put up again, and approving it ends the run', async () => {
      const second = await pendingFor(ctx.url, 2);
      const res = await resolve(ctx.url, second.id, 'approve', 'looks right');
      if (!res.ok) throw new Error(`approve: ${res.status} ${await res.text()}`);
      const outcome = await (await done!).text();
      const { transcript } = (await (await fetch(`${route(ctx.url)}/export`)).json()) as { transcript: unknown[] };
      const results = JSON.stringify(transcript);
      if (!/Rejected by[^"]*Add an optional stop hook to App/.test(results)) throw new Error(`no rejection fed back: ${results.slice(0, 3000)}`);
      if (!/passed\. Deliver version \d+\.\\nVersion 2 is approved\./.test(results)) throw new Error(`no approval note: ${results.slice(0, 3000)}`);
      console.log(`  v2 approval ${second.id} approved; run: ${outcome.slice(0, 200)}`);
    });
  },
};
export default scenario;

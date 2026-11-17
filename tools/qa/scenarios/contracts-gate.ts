import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { sessionDirName } from '@quack/distributor';
import { lastReviewBlock } from '@quack/contracts';
import type { FakeScript } from '../fake-model.ts';
import { answer, SDK_V1, SDK_V2, sdkSpec } from '../seed-contracts.ts';
import { visible, type Scenario } from '../scenario.ts';

// A QA contracts agent with the judge on: the judge's gate passes each version, then the contracts gate puts it to the person and waits.
const KEY = 'contracts:qa-gate';
// A second session, restarted while its version waits, under a spec file that no longer validates.
const RESTARTED = 'contracts:qa-gate-restart';
// A third, reviewed: comments on v2 are sent with the rejection, and v3 answers one by reply_comment and addresses the other.
const REVIEWED = 'contracts:qa-review';
const text = (m: { content?: unknown }) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? ''));
const write = (version: 1 | 2) => ({ tool: { name: 'write', arguments: { path: 'artifacts/spec.json', content: JSON.stringify(sdkSpec(version, version === 1 ? SDK_V1 : SDK_V2), null, 2) } } });
const writeSpec = (spec: unknown) => ({ tool: { name: 'write', arguments: { path: 'artifacts/spec.json', content: JSON.stringify(spec, null, 2) } } });
const SDK_V3 = SDK_V2.replace('stop?(): Promise<void>;', '/** Stops the app\'s background loops; the distributor awaits it on shutdown. */ stop?(): Promise<void>;');

// The reviewed session's writer: v1, then v2 with a baseline; after the review, a reply to the first comment and a v3 addressing the second.
function reviewRound(messages: Array<{ role: string; content?: unknown }>) {
  const last = messages.at(-1)!;
  if (last.role !== 'tool') return writeSpec(text(last).includes('stop hook') ? sdkSpec(2, SDK_V2, 1) : sdkSpec(1, SDK_V1));
  const result = text(last);
  if (/is approved\./.test(result)) return { content: 'Done.' };
  const review = messages.map(text).map((t) => lastReviewBlock(t.replace(/^Rejected by [^:]*: /, ''))).findLast(Boolean)?.block;
  if (/Rejected by/.test(result) && review) return { tool: { name: 'reply_comment', arguments: { id: review.comments[0]!.id, text: 'Optional, so extensions written before it still compile.' } } };
  if (/Replied to comment/.test(result) && review) {
    const v3 = sdkSpec(3, SDK_V3, 1);
    return writeSpec({ ...v3, changes: v3.changes!.map((c) => ({ ...c, addresses: [review.comments[1]!.id] })) });
  }
  return { tool: { name: 'submit_contract', arguments: {} } };
}

// Judge questions start "Decision:" and get the passing answer; the writer drafts v1, revises to v2 on a rejection, and stops once approved.
const script: FakeScript = ({ messages = [] }) => {
  const last = messages.at(-1);
  if (last?.role === 'user' && text(last).startsWith('Decision:')) return { content: JSON.stringify({ choice: answer(text(last)), confidence: 1, reason: 'qa' }) };
  if (messages.some((m) => m.role === 'user' && text(m).includes('review round'))) return reviewRound(messages);
  if (last?.role !== 'tool') return write(1);
  const result = text(last);
  if (/is approved\./.test(result)) return { content: 'Done.' };
  if (/Rejected by/.test(result)) return write(2);
  // A spec that no longer validates ends the turn; the contracts hook then asks for the passed version to be put up again.
  if (/does not match contract\.spec/.test(result)) return { content: 'The spec no longer validates; stopping here.' };
  return { tool: { name: 'submit_contract', arguments: {} } };
};

const agent = { definition: { name: 'qa-contracts', version: 1, model: 'local/qa-fake', prompt: 'prompt.md', extensions: { judge: { maxRevisions: 2 }, coding: {}, contracts: {} } }, prompt: 'You write artifacts/spec.json and call submit_contract.\n' };

type Ask = { id: string; state: string; toolCall?: { args?: { version?: number; artifact?: number } } };
const route = (url: string, key = KEY) => `${url}/sessions/${encodeURIComponent(key)}`;

// The pending approval for spec version `version`, once the contracts gate opens it; the fake model answers within a few seconds.
async function pendingFor(url: string, version: number, key = KEY, skip?: string): Promise<Ask> {
  for (let i = 0; i < 120; i++) {
    // Until the invocation creates the session, the route answers a problem rather than a list.
    const { asks = [] } = (await (await fetch(`${route(url, key)}/asks`)).json()) as { asks?: Ask[] };
    const found = asks.find((a) => a.state === 'pending' && a.toolCall?.args?.version === version && a.id !== skip);
    if (found) return found;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`no pending approval for version ${version}`);
}

const resolve = (url: string, id: string, choice: 'approve' | 'reject', reason: string, key = KEY) =>
  fetch(`${route(url, key)}/asks/${encodeURIComponent(id)}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ resolution: { choice, reason } }) });

const app = (url: string, path: string, init?: RequestInit) => fetch(`${url}/apps/contracts/contracts/${encodeURIComponent(REVIEWED)}${path}`, { ...init, headers: { 'content-type': 'application/json' } });
async function ok<T>(res: Promise<Response>, what: string): Promise<T> {
  const r = await res;
  if (!r.ok) throw new Error(`${what}: ${r.status} ${await r.text()}`);
  return (await r.json()) as T;
}
type Comment = { id: string; state: string; quote: string; replies: Array<{ text: string }>; addressedIn?: number; outdated?: { mark: string; in: number } };

async function reviewStep(url: string) {
  const invoke = (input: string) => fetch(`${route(url, REVIEWED)}/invocations?agent=qa-contracts`, { method: 'POST', body: JSON.stringify({ input, wait: true }) });
  const first = invoke('Define the extension SDK manifest types for a review round');
  await resolve(url, (await pendingFor(url, 1, REVIEWED)).id, 'approve', 'fine', REVIEWED);
  await first;
  const second = invoke('Add an optional stop hook to App');
  const v2 = await pendingFor(url, 2, REVIEWED);
  const artifact = v2.toolCall!.args!.artifact!;
  const post = (anchor: unknown, body: string) => ok<Comment>(app(url, '/comments', { method: 'POST', body: JSON.stringify({ artifact, anchor, body }) }), 'comment');
  const why = await post({ on: 'choice', id: 'stop' }, 'Why is stop optional?');
  const say = await post({ on: 'document', ptr: '/App' }, 'Say what stop must do.');
  if (!say.quote.includes('stop?(): Promise<void>')) throw new Error(`the app's quote of /App: ${say.quote}`);
  const review = await ok<{ reason: string }>(app(url, '/reviews', { method: 'POST', body: JSON.stringify({ note: 'Two things on the stop hook.', comments: [why.id, say.id] }) }), 'prepare');
  const rejected = await resolve(url, v2.id, 'reject', review.reason, REVIEWED);
  if (!rejected.ok) throw new Error(`reject: ${rejected.status} ${await rejected.text()}`);
  const v3 = await pendingFor(url, 3, REVIEWED);
  await resolve(url, v3.id, 'approve', 'answered', REVIEWED);
  await second;
  const { comments } = await ok<{ comments: Comment[] }>(app(url, '/comments'), 'list');
  const [a, b] = [comments.find((c) => c.id === why.id), comments.find((c) => c.id === say.id)];
  if (a?.state !== 'answered' || a.replies[0]?.text !== 'Optional, so extensions written before it still compile.' || a.outdated) throw new Error(`the replied comment: ${JSON.stringify(a)}`);
  const at = v3.toolCall!.args!.artifact!;
  if (b?.state !== 'addressed' || b.addressedIn !== at || b.outdated?.mark !== 'changed' || b.outdated.in !== at) throw new Error(`the addressed comment: ${JSON.stringify(b)}`);
  console.log(`  review ${why.id}: answered; ${say.id}: addressed and outdated in artifact ${b.addressedIn}`);
}

const scenario: Scenario = {
  name: 'contracts-gate',
  stack: { script, agents: { 'qa-contracts': agent }, apps: ['contracts'] },
  async run(ctx) {
    let done: Promise<Response> | undefined;
    await ctx.step('a passed version is put to the person, and a rejection goes back to the model as feedback', async () => {
      done = fetch(`${route(ctx.url)}/invocations?agent=qa-contracts`, { method: 'POST', body: JSON.stringify({ input: 'Define the extension SDK manifest types', wait: true }) });
      done.catch(() => undefined);
      const first = await pendingFor(ctx.url, 1);
      // The passed spec names the session before its approval opens, so the catalog row already carries the spec's title.
      const { title } = (await (await fetch(route(ctx.url))).json()) as { title?: string };
      if (title !== 'Extension SDK manifest') throw new Error(`session title is ${JSON.stringify(title)}, not the spec's`);
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
      if (!/passed\. Version 2 is final for this run; finish without revising it\.\\nVersion 2 is approved\./.test(results)) throw new Error(`no approval note: ${results.slice(0, 3000)}`);
      console.log(`  v2 approval ${second.id} approved; run: ${outcome.slice(0, 200)}`);
    });
    await ctx.step('after a restart whose rerun fails before its gate, the old ask is withdrawn, a late approve says why, and the version is asked again', async () => {
      void fetch(`${route(ctx.url, RESTARTED)}/invocations?agent=qa-contracts`, { method: 'POST', body: JSON.stringify({ input: 'Define the extension SDK manifest types' }) }).catch(() => undefined);
      const stale = await pendingFor(ctx.url, 1, RESTARTED);
      // A deploy with a stricter check, as in #2374: once killed, the process's rerun of submit_contract fails before its gate.
      const dir = sessionDirName(RESTARTED);
      writeFileSync(join(ctx.dataDir, dir, 'workspace', 'artifacts', 'spec.json'), '{}');
      process.kill((JSON.parse(readFileSync(join(ctx.dataDir, '.session-processes', `${dir}.json`), 'utf8')) as { pid: number }).pid, 'SIGKILL');
      const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
      try {
        const page = await context.newPage();
        // Opening the session starts its process again; the contracts hook then has the passed version put up once more.
        await page.goto(`${ctx.url}/#/s/${encodeURIComponent(RESTARTED)}`);
        const fresh = await pendingFor(ctx.url, 1, RESTARTED, stale.id);
        let ask = stale;
        for (let i = 0; i < 60 && ask.state !== 'cancelled'; i++, await new Promise((r) => setTimeout(r, 500))) ask = (await (await fetch(`${route(ctx.url, RESTARTED)}/asks/${encodeURIComponent(stale.id)}`)).json()) as Ask;
        const { pendingAsks } = (await (await fetch(route(ctx.url, RESTARTED))).json()) as { pendingAsks: number };
        if (ask.state !== 'cancelled' || pendingAsks !== 1) throw new Error(`the stale ask reads ${JSON.stringify(ask)}, pendingAsks ${pendingAsks}`);
        await ctx.shot(page, 'contracts-gate-restarted-session');
        await page.goto(`${ctx.url}/#/`);
        const cards = page.getByRole('region', { name: 'Needs you' }).getByRole('region', { name: 'Approval' });
        await visible(cards.first());
        if ((await cards.count()) !== 1) throw new Error(`Needs you offers ${await cards.count()} approvals, not the one fresh ask`);
        await ctx.shot(page, 'contracts-gate-restarted-home');
      } finally {
        await context.close();
      }
      const approve = (id: string) => {
        try {
          return { out: execFileSync(process.execPath, ['packages/cli/src/main.ts', '--url', ctx.url, 'approve', RESTARTED, id], { encoding: 'utf8', stdio: 'pipe' }) };
        } catch (err) {
          return { err: String((err as { stderr?: string }).stderr) };
        }
      };
      // The catalog holds the ask cancelled by now, so it refuses without asking the session for its reason.
      const late = approve(stale.id);
      if (!/the request was withdrawn: the session is no longer waiting for it/.test(late.err ?? '')) throw new Error(`quack approve of the stale ask: ${JSON.stringify(late)}`);
      const fresh = (await pendingFor(ctx.url, 1, RESTARTED, stale.id)).id;
      if (approve(fresh).err !== undefined) throw new Error(`quack approve of the fresh ask ${fresh} failed`);
      console.log(`  stale approval ${stale.id} withdrawn (quack approve: ${late.err!.trim()}); fresh approval ${fresh} approved`);
    });
    await ctx.step('a review round: comments on v2 go with the rejection, the agent replies to one and addresses the other, and v3 shows them answered and addressed', () => reviewStep(ctx.url));
  },
};
export default scenario;

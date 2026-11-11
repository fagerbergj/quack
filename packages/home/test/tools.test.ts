import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, before, test } from 'node:test';
import { obj, str } from '@earendil-works/pi-schema';
import { unescapeTerm, type Action } from '@quack/sdk';
import { distributor } from '../src/client.ts';
import { homeTools } from '../src/tools.ts';

const calls: Array<{ method: string; url: string; body: string }> = [];
const day = (n: number) => `2026-11-0${n}T12:00:00Z`;
const sessions = [
  { key: 'github:acme/widgets#7', agent: 'reviewer', title: 'Review acme/widgets#7', state: 'idle', pendingAsks: 1, createdAt: day(1), lastActivity: day(2) },
  { key: 'sleeper:L1:9:waivers', agent: 'sleeper-waivers', title: 'Waivers week 9', state: 'idle', pendingAsks: 0, lastRun: { outcome: 'failed', endedAt: day(3), failedSteps: [{ step: 'submit', error: 'deadline passed' }] }, createdAt: day(1), lastActivity: day(3) },
  { key: 'research:abc', agent: 'researcher', title: 'Why is the sky blue', state: 'running', pendingAsks: 0, createdAt: day(1), lastActivity: day(1) },
  { key: 'home:one', agent: 'quack', title: 'chat', state: 'running', pendingAsks: 0, createdAt: day(1), lastActivity: day(4) },
];
const long = 'x'.repeat(5000);

// The term a q sets for one field, unescaped; `-key:` (a negation) is not a `key:` term.
const termOf = (q: string, field: string) => {
  const m = new RegExp(String.raw`(?:^| )${field}:((?:\\.|\S)+)`).exec(q);
  return m ? unescapeTerm(m[1]!).text : undefined;
};

// The Sessions API as the tools use it: the q they send (key, agent, not home:) applied, two rows to a page so every list is followed.
function sessionPage(url: string) {
  const q = new URL(url, 'http://x').searchParams;
  const [agent, key] = [termOf(q.get('q') ?? '', 'agent'), termOf(q.get('q') ?? '', 'key')];
  const rows = sessions.filter((s) => (!q.get('q')?.includes(String.raw`-key:home\:*`) || !s.key.startsWith('home:')) && (!agent || s.agent === agent) && (!key || s.key === key));
  const from = Number(q.get('pageToken') ?? 0);
  return { sessions: rows.slice(from, from + 2), ...(from + 2 < rows.length && { nextPageToken: String(from + 2) }) };
}
const widgets = 'github:acme/widgets#7';
// The cross-session listings carry no artifact content.
const artifacts = [
  { session: widgets, kind: 'review', version: 2, summary: 'Requests changes: a nil check is missing', deliveries: [{ where: 'github', link: 'https://github.com/acme/widgets/pull/7#r1' }] },
  { session: widgets, kind: 'notes', version: 1 },
  { session: widgets, kind: 'extra', version: 1 },
  { session: widgets, kind: 'review', version: 1, summary: 'old' },
  { session: 'research:abc', kind: 'report', version: 1, summary: 'Rayleigh scattering' },
];
const asks = [
  { session: widgets, id: 'q1', state: 'pending', type: 'question', prompt: { question: 'Merge?', options: ['yes', 'no'] } },
  { session: widgets, id: 'a1', state: 'pending', type: 'approval', toolCall: { tool: 'merge_pr' } },
  { session: widgets, id: 'a0', state: 'resolved', type: 'approval', toolCall: { tool: 'push' } },
];
const listing = (url: string) => {
  const q = new URL(url, 'http://x').searchParams.get('q') ?? '';
  const mine = <T extends { session: string; state?: string }>(rows: T[]) => rows.filter((r) => r.session === termOf(q, 'session') && (!termOf(q, 'state') || r.state === termOf(q, 'state')));
  return url.startsWith('/artifacts?') ? { artifacts: mine(artifacts) } : { asks: mine(asks) };
};
const REPORT = '## Summary\nRayleigh scattering.\n\n## Findings\n- Air scatters blue light about 5.5 times more than red. [s1]';
// The export carries every version with its content, beside the transcript.
const reply: Record<string, unknown> = {
  'GET /sessions/github%3Aacme%2Fwidgets%237/messages?limit=6': { messages: [{ role: 'user', text: 'Review it' }, { role: 'assistant', text: long }] },
  'GET /sessions/github%3Aacme%2Fwidgets%237/export': { transcript: [], artifacts: [
    { kind: 'review', version: 1, content: 'old body' }, { kind: 'review', version: 2, content: 'y'.repeat(7000) }, { kind: 'notes', version: 1, content: 'plain notes body' }, { kind: 'extra', version: 1, content: 'e' },
  ] },
  'GET /sessions/research%3Aabc/export': { transcript: [], artifacts: [{ kind: 'report', version: 1, content: REPORT }] },
};

// The home agent's credential as the distributor holds it: listings and process routes reach any session, but the
// Sessions API's single-key reads answer 404 for every key but its own, and no test reads its own.
const server = createServer(async (req, res) => {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c);
  const url = req.url ?? '';
  calls.push({ method: req.method ?? '', url, body: Buffer.concat(chunks).toString() });
  const foreign = req.method === 'GET' && /^\/sessions\/[^/?]+(\/(artifacts|asks))?(\?|$)/.test(url);
  const found = foreign ? undefined : url.startsWith('/sessions?') ? sessionPage(url) : /^\/(artifacts|asks)\?/.test(url) ? listing(url) : reply[`${req.method} ${url}`];
  const post = req.method === 'POST' ? { ok: true } : undefined;
  res.writeHead(found || post ? 200 : 404, { 'content-type': 'application/json' }).end(JSON.stringify(found ?? post ?? { error: 'nope' }));
});
let base = '';
before(async () => {
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});
after(() => server.close());

const started: unknown[] = [];
const ping: Action<ReturnType<typeof obj>> = {
  name: 'ping_thing',
  summary: 'Pings a thing',
  params: obj({ thing: str() }, { open: true }),
  async start(params) {
    started.push(params);
    return { session: 'thing:1', title: 'Ping' };
  },
};
const tool = (name: string) => homeTools(distributor(base), [ping], { distributorUrl: base, fetch, env: {} }).find((t) => t.name === name)!;
const run = async (name: string, args: object) => {
  const out = await tool(name).execute(args as never, { taskId: 'task-1' } as never, {} as never);
  return { text: (out.output ?? []).map((p) => (p.type === 'text' ? p.text : '')).join(''), isError: out.isError };
};

test('find_sessions lists newest first with state, hides home chats, and filters', async () => {
  const all = (await run('find_sessions', {})).text.split('\n');
  assert.equal(all.length, 3);
  assert.match(all[0], /Waivers week 9 .* failed .*#\/s\/sleeper%3AL1%3A9%3Awaivers$/);
  assert.match(all[1], /Review acme\/widgets#7 .* needs_you /);
  assert.match(all[2], /Why is the sky blue .* running /);
  assert.match((await run('find_sessions', { state: 'needs_you' })).text, /^Review acme/);
  assert.match((await run('find_sessions', { text: 'WIDGETS' })).text, /^Review acme/);
  assert.equal((await run('find_sessions', { text: 'nothing' })).text, 'No sessions match.');
});

test('find_sessions asks the server to drop home chats and keep one agent, and follows every page', async () => {
  calls.length = 0;
  await run('find_sessions', { agent: 'researcher' });
  const asked = calls.filter((c) => c.url.startsWith('/sessions?')).map((c) => new URL(c.url, 'http://x').searchParams.get('q'));
  assert.deepEqual(asked, [String.raw`-key:home\:* AND agent:researcher`]);
  calls.length = 0;
  assert.equal((await run('find_sessions', {})).text.split('\n').length, 3);
  assert.equal(calls.filter((c) => c.url.startsWith('/sessions?')).length, 2, 'three rows over two-row pages');
});

test('read_session reads another session through the listings the home grant allows: newest results, what waits on you, last messages', async () => {
  calls.length = 0;
  const { text } = await run('read_session', { key: widgets });
  const [overview] = text.split('\n--- ');
  assert.ok(overview!.length <= 4001);
  assert.match(text, /state needs_you/);
  assert.match(text, /- review v2: Requests changes: a nil check is missing \(github https:/);
  assert.match(text, /- notes v1\n- extra v1\n/);
  assert.doesNotMatch(text, /review v1/);
  assert.doesNotMatch(text, /push/, 'resolved asks stay out');
  assert.match(text, /Waiting on you:\n- Merge\? \[yes \/ no\]\n- approve merge_pr\?/);
  assert.match(text, /- user: Review it/);
  assert.match(text, /\n--- review v2 ---\ny{6000}\n… \[1000 more characters; the whole review is at #\/s\/github%3Aacme%2Fwidgets%237\]\n--- notes v1 ---\nplain notes body\n--- extra v1 ---\ne$/);
  assert.doesNotMatch(text, /old body/, 'only the newest version of a kind');
  assert.ok(!calls.some((c) => /\/(ping|questions|failures)$/.test(c.url)), 'no read wakes the session');
});

test("read_session returns a researcher session's report text", async () => {
  const { text } = await run('read_session', { key: 'research:abc' });
  assert.match(text, /^Why is the sky blue \(researcher\), state running[\s\S]*- report v1: Rayleigh scattering/);
  assert.ok(text.endsWith(`--- report v1 ---\n${REPORT}`), text);
});

test("read_session shows a failed run's steps from the row, and skips the export when nothing is sealed", async () => {
  calls.length = 0;
  assert.match((await run('read_session', { key: 'sleeper:L1:9:waivers' })).text, /state failed[\s\S]*Failures:\n- submit: deadline passed/);
  assert.ok(!calls.some((c) => c.url.endsWith('/export')));
});

test('a key the distributor does not list is refused before any session endpoint is touched', async () => {
  calls.length = 0;
  for (const name of ['read_session', 'send_message', 'stop_session', 'run_again']) {
    const out = await run(name, { key: 'nope:1', message: 'hi' });
    assert.equal(out.isError, true);
    assert.match(out.text, /no session with key nope:1/);
  }
  assert.equal((await run('stop_session', { key: 'home:one' })).isError, true);
  assert.ok(calls.every((c) => c.url.startsWith('/sessions?')));
});

test('send_message queues behind the current step under the session\'s own agent', async () => {
  calls.length = 0;
  await run('send_message', { key: 'github:acme/widgets#7', message: 'check the tests too' });
  const post = calls.find((c) => c.method === 'POST')!;
  assert.equal(post.url, '/sessions/github%3Aacme%2Fwidgets%237/invocations?agent=reviewer');
  assert.deepEqual(JSON.parse(post.body), { input: 'check the tests too', requestId: 'home:task-1', whenBusy: 'followUp' });
});

test('stop_session aborts and run_again resumes the named session', async () => {
  calls.length = 0;
  await run('stop_session', { key: 'github:acme/widgets#7' });
  await run('run_again', { key: 'github:acme/widgets#7' });
  assert.deepEqual(calls.filter((c) => c.method === 'POST').map((c) => c.url), ['/sessions/github%3Aacme%2Fwidgets%237/abort', '/sessions/github%3Aacme%2Fwidgets%237/resume']);
});

test('start refuses an unknown action or bad params without asking or starting anything', async () => {
  started.length = 0;
  assert.match((await run('start', { action: 'nope', params: {} })).text, /unknown action nope; use one of ping_thing/);
  const bad = await run('start', { action: 'ping_thing', params: {} });
  assert.equal(bad.isError, true);
  assert.match(bad.text, /bad params for ping_thing/);
  assert.deepEqual(started, []);
});

test('start lists each action with its summary and params for the model', () => {
  assert.match(tool('start').description, /- ping_thing: Pings a thing\. Params \{.*"thing"/);
});

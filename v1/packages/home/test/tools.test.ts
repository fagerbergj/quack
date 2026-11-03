import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, before, test } from 'node:test';
import { obj, str } from '@earendil-works/pi-schema';
import type { Action } from '@quack/sdk';
import { distributor } from '../src/client.ts';
import { homeTools } from '../src/tools.ts';

const calls: Array<{ method: string; url: string; body: string }> = [];
const day = (n: number) => `2026-11-0${n}T12:00:00Z`;
const sessions = [
  { key: 'github:acme/widgets#7', agent: 'reviewer', title: 'Review acme/widgets#7', running: true, active: false, createdAt: day(1), lastActivity: day(2) },
  { key: 'sleeper:L1:9:waivers', agent: 'sleeper-waivers', title: 'Waivers week 9', running: true, active: true, createdAt: day(1), lastActivity: day(3) },
  { key: 'research:abc', agent: 'researcher', title: 'Why is the sky blue', running: false, active: false, createdAt: day(1), lastActivity: day(1) },
  { key: 'home:one', agent: 'quack', title: 'chat', running: true, active: true, createdAt: day(1), lastActivity: day(4) },
];
const long = 'x'.repeat(5000);
const reply: Record<string, unknown> = {
  'GET /sessions': { sessions },
  'GET /index/questions?state=pending': { questions: [{ session: 'research:abc' }] },
  'GET /index/approvals?state=pending': { approvals: [] },
  'GET /index/failures': { failures: [{ session: 'sleeper:L1:9:waivers' }] },
  'GET /sessions/github%3Aacme%2Fwidgets%237/ping': { status: 'Healthy' },
  'GET /sessions/github%3Aacme%2Fwidgets%237/artifacts': {
    artifacts: [
      { kind: 'review', version: 1, content: 'old', verdict: null },
      { kind: 'review', version: 2, content: 'Verdict: request changes\nbecause...', summary: 'Requests changes: a nil check is missing', verdict: { passed: true }, links: [{ label: 'Posted', url: 'https://github.com/acme/widgets/pull/7#r1' }] },
      { kind: 'notes', version: 1, content: 'plain notes body', verdict: null },
      { kind: 'extra', version: 1, content: 'e', verdict: null },
    ],
  },
  'GET /sessions/github%3Aacme%2Fwidgets%237/questions': { questions: [{ question: 'Merge?', options: ['yes', 'no'] }] },
  'GET /sessions/github%3Aacme%2Fwidgets%237/failures': { failures: [] },
  'GET /sessions/github%3Aacme%2Fwidgets%237/messages?limit=6': { messages: [{ role: 'user', text: 'Review it' }, { role: 'assistant', text: long }] },
};

const server = createServer(async (req, res) => {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c);
  const url = req.url ?? '';
  calls.push({ method: req.method ?? '', url, body: Buffer.concat(chunks).toString() });
  const found = reply[`${req.method} ${url}`];
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
  assert.match(all[1], /Review acme\/widgets#7 .* done /);
  assert.match(all[2], /needs_you/);
  assert.match((await run('find_sessions', { state: 'needs_you' })).text, /^Why is the sky blue/);
  assert.match((await run('find_sessions', { text: 'WIDGETS' })).text, /^Review acme/);
  assert.equal((await run('find_sessions', { text: 'nothing' })).text, 'No sessions match.');
});

test('read_session returns a bounded read: newest results with summaries, questions, last messages', async () => {
  const { text } = await run('read_session', { key: 'github:acme/widgets#7' });
  assert.ok(text.length <= 4001);
  assert.match(text, /state needs_you/);
  assert.match(text, /- review v2, checks passed: Requests changes: a nil check is missing \(Posted https:/);
  assert.match(text, /- notes v1: plain notes body/);
  assert.doesNotMatch(text, /review v1/);
  assert.match(text, /Open questions:\n- Merge\? \[yes \/ no\]/);
  assert.match(text, /- user: Review it/);
});

test('a key the distributor does not list is refused before any session endpoint is touched', async () => {
  calls.length = 0;
  for (const name of ['read_session', 'send_message', 'stop_session', 'run_again']) {
    const out = await run(name, { key: 'nope:1', message: 'hi' });
    assert.equal(out.isError, true);
    assert.match(out.text, /no session with key nope:1/);
  }
  assert.equal((await run('stop_session', { key: 'home:one' })).isError, true);
  assert.ok(calls.every((c) => c.url === '/sessions'));
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

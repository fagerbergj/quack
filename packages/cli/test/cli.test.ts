import assert from 'node:assert/strict';
import { chmod, mkdir, mkdtemp, readFile, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer, type IncomingMessage, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, afterEach, before, beforeEach, describe, it } from 'node:test';
import { WebSocketServer } from 'ws';
import { client } from '../src/api.ts';
import { run } from '../src/main.ts';

const money = { currencyCode: 'USD', amountMicros: '0' };
const tokens = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
const row = (key: string, agent: string, state: string, extra: object = {}) => ({ key, agent, state, createdAt: 't', lastActivity: 't', cost: money, tokens, pendingAsks: 0, ...extra });
const sessions = [
  row('ui:a', 'chat', 'running', { title: 'Alpha' }),
  row('ui:b', 'review', 'idle', { pendingAsks: 2, oldestAskAt: 't' }),
  row('ui:c', 'chat', 'stopped', { lastRun: { outcome: 'failed', endedAt: 't', failedSteps: [] } }),
];
const asks = [
  { session: 'ui:b', id: 'ap1', type: 'approval', state: 'pending', createdAt: '1970-01-01T00:00:00.000Z', toolCall: { tool: 'bash', args: {} } },
  { session: 'ui:b', id: 'q1', type: 'question', state: 'pending', createdAt: '1970-01-01T00:00:01.000Z', prompt: { question: 'Which?', options: ['this one', 'that'], freeText: true } },
];
// Newest first, as the per-session list orders them.
const sealed = [
  { session: 'ui:a', kind: 'plan', version: 2, hash: 'h2', content: 'new plan', sealedAt: 't2', summary: 's2' },
  { session: 'ui:a', kind: 'plan', version: 1, hash: 'h1', content: 'old plan', sealedAt: 't1', summary: 's1' },
];
const messages = [{ role: 'user', text: 'hello' }, { role: 'assistant', text: 'hi there' }];
const check = (id: string, weight: number | 'gate', met: number | null, notes: string[] = []) => ({ criterion: 'sound', id, label: `${id} label`, weight, met, notes });
// The socket's ledger: every version with its verdict, null for one the judge has not given.
const ledger = [
  { ...sealed[1], verdict: { passed: true, limitReached: false, score: 1, gatesFailed: [], rubric: 'plan@1', checks: [] } },
  { ...sealed[0], verdict: {
    passed: false, limitReached: false, score: 0.6666667, gatesFailed: ['sound.clean'], rubric: 'plan@2',
    checks: [check('clean', 'gate', 0, [`line one\n  ${'x'.repeat(200)}`, 'second note']), check('kept', 'gate', 1), check('extra', -3, 1), check('moot', 2, null), check('says_why', 2, 0.5)],
  } },
  { session: 'ui:a', kind: 'notes', version: 1, hash: 'h3', content: 'n', sealedAt: 't3', verdict: null },
];
const contractRows = { contracts: [{ key: 'contracts:x', title: 'Sessions API', approved: 10, pendingApproval: true }], nextPageToken: 'n/1' };

let server: Server;
let url: string;
let calls: Array<{ method: string; path: string; query: Record<string, string>; body: unknown; auth?: string }>;
let frames: unknown[];
let queue: Array<{ id: string; text: string }>;
let streaming = false;
let defaultSize = 100;
let verdictless = false;
let sockets = 0;
let xdg: string;

// One page of `rows` for the request's pageSize and pageToken, as every v9 list answers.
function page(field: string, rows: unknown[], q: URLSearchParams) {
  const from = Number(q.get('pageToken') ?? 0);
  const size = Number(q.get('pageSize') ?? defaultSize);
  return { [field]: rows.slice(from, from + size), ...(from + size < rows.length && { nextPageToken: String(from + size) }) };
}

// The fake's stand-in for the server's q: just the kind and version terms the artifact command sends.
const artifactsFor = (q: string) => sealed.filter((a) => (!/kind:/.test(q) || q.includes(`kind:${a.kind}`)) && (!/version:/.test(q) || q.includes(`version:${a.version}`)));

const readBody = async (req: IncomingMessage) => { let s = ''; for await (const c of req) s += c; return s ? JSON.parse(s) : undefined; };

const ROUTES: Array<[string, RegExp, (m: RegExpExecArray, q: URLSearchParams) => [number, object]]> = [
  ['GET', /^\/sessions$/, (_, q) => [200, page('sessions', sessions, q)]],
  ['GET', /^\/sessions\/([^/]+)$/, ([, k]) => { const s = sessions.find((x) => x.key === k); return s ? [200, s] : [404, { title: 'Not Found', status: 404, detail: 'no such session' }]; }],
  ['GET', /^\/asks$/, (_, q) => [200, page('asks', asks, q)]],
  ['GET', /^\/sessions\/([^/]+)\/asks$/, ([, k], q) => [200, page('asks', asks.filter((a) => a.session === k), q)]],
  ['PATCH', /^\/sessions\/[^/]+\/asks\/ap9$/, () => [409, { title: 'Conflict', status: 409, detail: 'the ask is not pending' }]],
  ['PATCH', /^\/sessions\/[^/]+\/asks\/([^/]+)$/, ([, id]) => [200, { ...asks.find((a) => a.id === id), state: 'resolved' }]],
  ['GET', /^\/artifacts$/, (_, q) => [200, page('artifacts', sealed.map(({ content: _c, ...meta }) => meta), q)]],
  ['GET', /^\/sessions\/[^/]+\/artifacts$/, (_, q) => [200, page('artifacts', artifactsFor(q.get('q') ?? ''), q)]],
  ['GET', /^\/sessions\/[^/]+\/messages$/, (_, q) => [200, { messages: messages.slice(-Number(q.get('limit'))) }]],
  ['GET', /^\/sessions\/[^/]+\/export$/, () => [200, { session: sessions[0], transcript: [], artifacts: sealed }]],
  ['GET', /^\/version$/, () => [200, { commit: 'abcdef1234567890', builtAt: 't' }]],
  ['GET', /^\/agents$/, () => [200, { agents: [{ name: 'chat', version: 3 }] }]],
  ['DELETE', /^\/sessions\/ui:b$/, () => [409, { error: 'session is running' }]],
  ['GET', /^\/apps\/contracts\/contracts$/, () => [200, contractRows]],
  ['GET', /^\/apps\/contracts\/contracts\/([^/]+)$/, ([, k]) => [200, { key: k, versions: [{ version: 1 }] }]],
  ['GET', /^\/apps\/nope\//, () => [404, { title: 'Not Found', status: 404, detail: 'no app nope' }]],
];

before(async () => {
  server = createServer(async (req, res) => {
    const u = new URL(req.url!, 'http://x');
    const path = decodeURIComponent(u.pathname);
    const call = { method: req.method!, path, query: Object.fromEntries(u.searchParams), body: await readBody(req), auth: req.headers.authorization };
    calls.push(call);
    const reply = (status: number, body: object) => { res.writeHead(status, { 'content-type': status < 400 ? 'application/json' : 'application/problem+json' }); res.end(JSON.stringify(body)); };
    if (path.endsWith('/invocations')) {
      const body = call.body as { input: string; wait?: boolean };
      return reply(body.wait ? 200 : 202, body.input === 'fail' ? { status: 'unanswered', reason: 'model_fault' } : body.wait ? { status: 'done', answer: 'final answer' } : { status: 'running' });
    }
    for (const [method, pattern, route] of ROUTES) {
      const m = req.method === method ? pattern.exec(path) : null;
      if (m) return reply(...route(m, u.searchParams));
    }
    reply(200, { key: 'ok' });
  });
  const wss = new WebSocketServer({ server });
  wss.on('connection', (ws) => {
    sockets += 1;
    const entry = (role: string, text: string) => ({ kind: 'entry', entry: { model: [{ role, content: [{ type: 'text', text }] }] } });
    if (streaming) {
      ws.send(JSON.stringify({ type: 'view', items: [entry('user', 'go')], live: { run: {} } }));
      ws.send(JSON.stringify({ type: 'view', items: [entry('user', 'go'), entry('assistant', 'streamed one')], live: { run: {} } }));
      ws.send(JSON.stringify({ type: 'view', live: {} }));
      return;
    }
    ws.send(JSON.stringify({ type: 'view', items: [entry('user', 'hello')], live: {} }));
    ws.send(JSON.stringify({ type: 'queued', items: queue }));
    ws.send(JSON.stringify({ type: 'artifacts', artifacts: verdictless ? ledger.map((a) => ({ ...a, verdict: null })) : ledger }));
    ws.on('message', (d) => {
      const frame = JSON.parse(String(d));
      frames.push(frame);
      // The session server answers a withdraw with no frame of its own; only the queue it pushes changes.
      if (frame.type === 'withdraw') return ws.send(JSON.stringify({ type: 'queued', items: (queue = queue.filter((q) => q.id !== frame.id)) }));
      ws.send(JSON.stringify(frame.input === 'bad' ? { type: 'error', error: 'rejected' } : { type: 'accepted', submissionId: 'f2', status: 'queued' }));
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});
after(() => server.close());
beforeEach(async () => { xdg = await mkdtemp(join(tmpdir(), 'quack-xdg-')); calls = []; frames = []; sockets = 0; queue = [{ id: 'f1', text: 'later' }]; });
afterEach(() => assert.deepEqual(calls.filter((c) => c.path.startsWith('/index')), [], 'the index routes are retired'));

async function cli(args: string[], env: Record<string, string> = {}) {
  let stdout = '';
  let stderr = '';
  const code = await run(args, { QUACK_URL: url, XDG_CONFIG_HOME: xdg, ...env }, { stdout: (s) => (stdout += s), stderr: (s) => (stderr += s) });
  return { code, stdout, stderr };
}

const qOf = async (args: string[]) => { calls = []; await cli(args); return calls[0]!.query.q; };

describe('quack', () => {
  it('lists commands for --help', async () => {
    const r = await cli(['--help']);
    assert.equal(r.code, 0);
    for (const c of ['sessions', 'show', 'new', 'send', 'steer', 'withdraw', 'stop', 'resume', 'rename', 'archive', 'unarchive', 'delete', 'export', 'asks', 'answer', 'approvals', 'approve', 'reject', 'artifacts', 'artifact', 'app', 'agents', 'completion', 'eval']) assert.match(r.stdout, new RegExp(`quack ${c}\\b`));
  });

  it('sessions prints the API page unchanged with --json and drops archived ones by default', async () => {
    const r = await cli(['sessions', '--json']);
    assert.deepEqual(JSON.parse(r.stdout), { sessions });
    assert.deepEqual(calls.map((c) => [c.path, c.query]), [['/sessions', { q: '-state:archived' }]]);
    assert.match((await cli(['sessions'])).stdout, /ui:a\s+chat\s+running\s+0\s+Alpha/);
  });

  it('sessions maps --state and --agent to q, escaping the agent', async () => {
    assert.equal(await qOf(['sessions', '--state', 'running']), '-state:archived AND state:running');
    assert.equal(await qOf(['sessions', '--state', 'needs_you']), '-state:archived AND pendingAsks:>0');
    assert.equal(await qOf(['sessions', '--state', 'failed']), '-state:archived AND lastRun.outcome:failed');
    assert.equal(await qOf(['sessions', '--state', 'done']), '-state:archived AND (state:idle OR state:stopped) AND pendingAsks:0');
    assert.equal(await qOf(['sessions', '--agent', 'chat']), '-state:archived AND agent:chat');
    assert.equal(await qOf(['sessions', '--agent', 'a:b OR x']), String.raw`-state:archived AND agent:a\:b\ OR\ x`);
    assert.equal((await cli(['sessions', '--state', 'nope'])).code, 1);
  });

  it('sessions takes a q of its own in place of the archived default', async () => {
    assert.equal(await qOf(['sessions', 'state:archived']), '(state:archived)');
    assert.equal(await qOf(['sessions', '--agent', 'chat', 'title:x', 'OR', 'title:y']), '(title:x OR title:y) AND agent:chat');
  });

  it('sessions --order-by, --page-size and --page-token page the list, and the next token goes to stderr', async () => {
    const r = await cli(['sessions', '--keys', '--page-size', '2', '--order-by', 'oldestAskAt']);
    assert.deepEqual([r.stdout, r.stderr], ['ui:a\nui:b', 'next page: --page-token 2']);
    assert.deepEqual(calls[0]!.query, { q: '-state:archived', orderBy: 'oldestAskAt', pageSize: '2' });
    calls = [];
    const last = await cli(['sessions', '--keys', '--page-size', '2', '--page-token', '2']);
    assert.deepEqual([last.stdout, last.stderr], ['ui:c', '']);
    assert.equal(calls.at(0)!.query.pageToken, '2');
    assert.equal(JSON.parse((await cli(['sessions', '--json', '--page-size', '2'])).stdout).nextPageToken, '2');
  });

  it('sessions --keys prints one key per line', async () => {
    assert.equal((await cli(['sessions', '--keys'])).stdout, 'ui:a\nui:b\nui:c');
  });

  it('asks lists pending approvals and questions across sessions or for one, with a q of its own', async () => {
    const r = await cli(['asks']);
    assert.match(r.stdout, /ui:b\s+ap1\s+approval\s+1970-01-01T00:00:00.000Z\s+bash/);
    assert.match(r.stdout, /ui:b\s+q1\s+question\s+\S+\s+Which\?/);
    assert.deepEqual(calls.map((c) => [c.path, c.query]), [['/asks', { q: 'state:pending' }]]);
    calls = [];
    assert.deepEqual(JSON.parse((await cli(['asks', '--json', '--session', 'ui:b', 'type:approval'])).stdout), { asks });
    assert.deepEqual(calls.map((c) => [c.path, c.query]), [['/sessions/ui:b/asks', { q: 'state:pending AND (type:approval)' }]]);
    assert.equal(await qOf(['approvals']), 'state:pending AND (type:approval)');
    assert.match((await cli(['asks', '--page-size', '1'])).stderr, /next page: --page-token 1/);
  });

  it('answer resolves either kind with the nested body; approve and reject are its aliases', async () => {
    assert.equal((await cli(['answer', 'ui:b', 'q1', 'this', 'one', '--text', 'free', '--reason', 'why'])).code, 0);
    await cli(['approve', 'ui:b', 'ap1']);
    await cli(['reject', 'ui:b', 'ap1', 'too', 'risky']);
    await cli(['reject', 'ui:b', 'ap1', '--reason', 'flagged']);
    assert.deepEqual(calls.map((c) => [c.method, c.path, c.body]), [
      ['PATCH', '/sessions/ui:b/asks/q1', { resolution: { choice: 'this one', text: 'free', reason: 'why' } }],
      ['PATCH', '/sessions/ui:b/asks/ap1', { resolution: { choice: 'approve' } }],
      ['PATCH', '/sessions/ui:b/asks/ap1', { resolution: { choice: 'reject', reason: 'too risky' } }],
      ['PATCH', '/sessions/ui:b/asks/ap1', { resolution: { choice: 'reject', reason: 'flagged' } }],
    ]);
    assert.equal((await cli(['answer', 'ui:b', 'q1'])).code, 1);
  });

  it('answer prints the problem detail when the ask is not pending', async () => {
    const stale = await cli(['approve', 'ui:b', 'ap9']);
    assert.equal(stale.code, 1);
    assert.match(stale.stderr, /the ask is not pending/);
  });

  it('show gathers the session, its pending asks, newest artifacts and last messages', async () => {
    const r = await cli(['show', 'ui:a']);
    assert.equal(r.code, 0);
    assert.match(r.stdout, /^ui:a {2}chat {2}running {2}Alpha/);
    assert.match(r.stdout, /plan\s+v2\s+t2\s+s2/);
    assert.doesNotMatch(r.stdout, /v1/);
    assert.match(r.stdout, /assistant: hi there/);
    // show reads the session, then the rest at once: they reach the server in any order.
    assert.deepEqual(calls.map((c) => [c.path, c.query] as const).sort(([x], [y]) => x.localeCompare(y)), [
      ['/sessions/ui:a', {}],
      ['/sessions/ui:a/artifacts', {}],
      ['/sessions/ui:a/asks', { q: 'state:pending' }],
      ['/sessions/ui:a/messages', { limit: '5' }],
    ]);
    const j = JSON.parse((await cli(['show', 'ui:b', '--json'])).stdout);
    assert.deepEqual(j.asks, asks);
    assert.deepEqual(j.messages, messages);
    assert.match((await cli(['show', 'ui:b'])).stdout, /q1\s+question\s+\S+\s+Which\?/);
  });

  it('show --verdict prints each newest version\'s verdict from the socket ledger, and --json carries them', async () => {
    assert.doesNotMatch((await cli(['show', 'ui:a'])).stdout, /Verdicts/);
    assert.equal(sockets, 0, 'a plain show opens no socket, so it starts no stopped session');
    const r = await cli(['show', '--verdict', 'ui:a']);
    assert.equal(sockets, 1);
    assert.equal(r.code, 0);
    const block = r.stdout.slice(r.stdout.indexOf('Verdicts'), r.stdout.indexOf('\n\nAsks'));
    assert.deepEqual(block.split('\n'), [
      'Verdicts',
      'plan v2  failed  score 0.67  plan@2  gates failed: sound.clean',
      `  sound.clean: line one ${'x'.repeat(110)}…`,
      '  sound.extra: extra label',
      '  sound.says_why: says_why label',
    ]);
    const j = JSON.parse((await cli(['show', '--verdict', '--json', 'ui:a'])).stdout);
    assert.deepEqual(j.verdicts, [{ kind: 'plan', version: 2, verdict: ledger[1]!.verdict }]);
    assert.equal(JSON.parse((await cli(['show', '--json', 'ui:a'])).stdout).verdicts, undefined);
  });

  it('show --verdict leaves out the ledger\'s nulls, which an unjudged kind carries too', async () => {
    verdictless = true;
    try {
      assert.match((await cli(['show', '--verdict', 'ui:a'])).stdout, /Verdicts\nnone\n/);
    } finally {
      verdictless = false;
    }
  });

  it('show --messages N prints the last N messages and refuses what the server would cut', async () => {
    const r = await cli(['show', 'ui:a', '--messages', '1']);
    assert.match(r.stdout, /Last messages\nassistant: hi there$/);
    assert.equal(calls.find((c) => c.path === '/sessions/ui:a/messages')!.query.limit, '1');
    for (const n of ['0', '51', 'x']) assert.equal((await cli(['show', 'ui:a', '--messages', n])).code, 1);
  });

  it('show follows the asks pages so nothing pending is left out', async () => {
    defaultSize = 1;
    try {
      assert.deepEqual(JSON.parse((await cli(['show', 'ui:b', '--json'])).stdout).asks.map((a: { id: string }) => a.id), ['ap1', 'q1']);
      assert.deepEqual(calls.filter((c) => c.path.endsWith('/asks')).map((c) => c.query.pageToken), [undefined, '1']);
    } finally {
      defaultSize = 100;
    }
  });

  it('exits 2 for an unknown session without touching it', async () => {
    const r = await cli(['show', 'ui:nope']);
    assert.equal(r.code, 2);
    assert.match(r.stderr, /no such session/);
    assert.deepEqual(calls.map((c) => c.path), ['/sessions/ui:nope']);
  });

  it('send returns the submission id once the server accepts, and reports a rejection', async () => {
    const r = await cli(['send', 'ui:a', 'do', 'it']);
    assert.deepEqual([r.code, r.stdout], [0, 'f2']);
    assert.deepEqual(frames, [{ type: 'input', input: 'do it' }]);
    const bad = await cli(['send', 'ui:a', 'bad']);
    assert.equal(bad.code, 1);
    assert.match(bad.stderr, /rejected/);
  });

  it('steer sends the input into the running turn as a steer', async () => {
    const r = await cli(['steer', 'ui:a', 'look', 'here']);
    assert.deepEqual([r.code, r.stdout], [0, 'f2']);
    assert.deepEqual(frames, [{ type: 'input', input: 'look here', whenBusy: 'steer' }]);
    assert.equal((await cli(['steer', 'ui:a'])).code, 1);
    assert.equal((await cli(['steer', 'ui:nope', 'x'])).code, 2);
  });

  it('withdraw removes a queued follow-up and exits 2 for one that is not queued', async () => {
    assert.equal((await cli(['withdraw', 'ui:a', 'f1'])).code, 0);
    assert.deepEqual(frames, [{ type: 'withdraw', id: 'f1' }]);
    frames = [];
    const gone = await cli(['withdraw', 'ui:a', 'f1']);
    assert.equal(gone.code, 2);
    assert.match(gone.stderr, /no queued message f1 in ui:a/);
    assert.deepEqual(frames, []);
  });

  it('stop, resume and housekeeping call the session routes, archiving through state', async () => {
    await cli(['stop', 'ui:a']);
    await cli(['resume', 'ui:a']);
    await cli(['rename', 'ui:a', 'New', 'name']);
    await cli(['archive', 'ui:a']);
    await cli(['unarchive', 'ui:a']);
    await cli(['delete', 'ui:a']);
    const writes = calls.filter((c) => c.method !== 'GET').map((c) => [c.method, c.path, c.body]);
    assert.deepEqual(writes, [
      ['POST', '/sessions/ui:a/abort', undefined],
      ['POST', '/sessions/ui:a/resume', undefined],
      ['PATCH', '/sessions/ui:a', { title: 'New name' }],
      ['PATCH', '/sessions/ui:a', { state: 'archived' }],
      ['PATCH', '/sessions/ui:a', { state: 'stopped' }],
      ['DELETE', '/sessions/ui:a', undefined],
    ]);
  });

  it('new starts a session through invocations and prints its key, minted under the agent\'s name', async () => {
    const r = await cli(['new', '--agent', 'contracts', 'plan', 'it']);
    assert.equal(r.code, 0);
    assert.match(r.stdout, /^contracts:[0-9a-f-]{36}$/);
    const post = calls.find((c) => c.method === 'POST')!;
    assert.deepEqual([post.path, post.query], [`/sessions/${r.stdout}/invocations`, { agent: 'contracts' }]);
    assert.equal((post.body as { input: string }).input, 'plan it');
    assert.match((await cli(['new', 'hi'])).stdout, /^ui:[0-9a-f-]{36}$/);
    assert.equal((await cli(['new'])).code, 1);
  });

  it('new --key starts the session at that key and refuses one that exists', async () => {
    const r = await cli(['new', '--key', 'contracts:mine', '--agent', 'contracts', 'go']);
    assert.deepEqual([r.code, r.stdout], [0, 'contracts:mine']);
    assert.deepEqual(calls.map((c) => [c.method, c.path]), [['GET', '/sessions/contracts:mine'], ['POST', '/sessions/contracts:mine/invocations']]);
    calls = [];
    const taken = await cli(['new', '--key', 'ui:a', 'go']);
    assert.equal(taken.code, 1);
    assert.match(taken.stderr, /session ui:a exists; send to it with quack send/);
    assert.deepEqual(calls.map((c) => c.method), ['GET']);
  });

  it('app GETs an extension app\'s route, as a table when it is one list of flat rows', async () => {
    const r = await cli(['app', 'contracts', '/contracts?q=pendingApproval:true']);
    assert.equal(r.code, 0);
    assert.deepEqual([calls[0]!.path, calls[0]!.query], ['/apps/contracts/contracts', { q: 'pendingApproval:true' }]);
    assert.match(r.stdout, /^KEY\s+TITLE\s+APPROVED\s+PENDINGAPPROVAL\ncontracts:x\s+Sessions API\s+10\s+true$/);
    assert.equal(r.stderr, 'next page: add pageToken=n%2F1 to the path');
    assert.deepEqual(JSON.parse((await cli(['app', 'contracts', 'contracts/contracts:x'])).stdout), { key: 'contracts:x', versions: [{ version: 1 }] });
    assert.deepEqual(JSON.parse((await cli(['app', 'contracts', '/contracts', '--json'])).stdout), contractRows);
    const missing = await cli(['app', 'nope', '/x']);
    assert.deepEqual([missing.code, missing.stderr], [2, 'quack: no app nope']);
    assert.equal((await cli(['app', 'contracts'])).code, 1);
    calls = [];
    await cli(['app', 'contracts', '/contracts'], { QUACK_ADMIN_TOKEN: 'tok' });
    assert.deepEqual(calls.map((c) => [c.path, c.auth]), [['/apps/contracts/contracts', undefined]], 'the app gateway 401s any bearer but a launch credential');
  });

  it('-p waits for the run and prints only the answer, failing non-zero when it does not finish', async () => {
    const n = await cli(['new', '-p', '--agent', 'chat', 'hi']);
    assert.deepEqual([n.code, n.stdout], [0, 'final answer']);
    assert.match(n.stderr, /^session chat:/);
    assert.equal((calls.find((c) => c.method === 'POST')!.body as { wait: boolean }).wait, true);
    const s = await cli(['send', '-p', 'ui:a', 'hi']);
    assert.deepEqual([s.code, s.stdout], [0, 'final answer']);
    assert.deepEqual(frames, []);
    const bad = await cli(['send', '-p', 'ui:a', 'fail']);
    assert.equal(bad.code, 1);
    assert.match(bad.stderr, /run unanswered: model_fault/);
    assert.equal(bad.stdout, '');
  });

  it('show -f prints new transcript lines until the run ends, and returns at once for an idle session', async () => {
    assert.equal((await cli(['show', 'ui:a', '-f'])).code, 0);
    streaming = true;
    try {
      const r = await cli(['show', '-f', 'ui:a']);
      assert.equal(r.code, 0);
      assert.doesNotMatch(r.stdout, /user: go/);
      assert.match(r.stdout, /assistant: hi there\s*assistant: streamed one$/);
    } finally {
      streaming = false;
    }
  });

  it('completion prints a static script and rejects other shells', async () => {
    const bash = (await cli(['completion', 'bash'])).stdout;
    assert.match(bash, /complete -F _quack quack/);
    assert.match(bash, /compgen -W "sessions show new send /);
    assert.match(bash, /show\|/);
    assert.match(bash, /--server \]; then COMPREPLY=\(\$\(compgen -W "\$\(quack server list --keys/);
    assert.match((await cli(['completion', 'zsh'])).stdout, /bashcompinit/);
    assert.equal((await cli(['completion', 'fish'])).code, 1);
  });

  it('eval hands its arguments to the eval package untouched', async () => {
    const r = await cli(['eval', '--concurrency=2', 'dataset'], { LANGFUSE_BASE_URL: '', PATH: process.env.PATH ?? '' });
    assert.equal(r.code, 1);
  });

  it('export writes the session file, named from the key by default', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'quack-export-'));
    const named = join(dir, 'out.json');
    const r = await cli(['export', 'ui:a', named]);
    assert.equal(r.code, 0);
    assert.deepEqual(JSON.parse(await readFile(named, 'utf8')).artifacts, sealed);
    assert.equal((await cli(['export', 'ui:nope', named])).code, 2);
    const cwd = process.cwd();
    process.chdir(dir);
    try {
      assert.match((await cli(['export', 'ui:a'])).stdout, /wrote quack-ui-a\.json/);
      await readFile(join(dir, 'quack-ui-a.json'));
    } finally {
      process.chdir(cwd);
    }
  });

  it('turns an API refusal into exit 1 with its message', async () => {
    const r = await cli(['delete', 'ui:b']);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /session is running/);
  });

  it('artifacts lists one page across sessions or for one session', async () => {
    assert.match((await cli(['artifacts'])).stdout, /ui:a\s+plan\s+v2\s+t2\s+s2/);
    assert.match((await cli(['artifacts', 'ui:a'])).stdout, /ui:a\s+plan\s+v1\s+t1\s+s1/);
    assert.deepEqual(calls.map((c) => c.path), ['/artifacts', '/sessions/ui:a/artifacts']);
    assert.match((await cli(['artifacts', '--page-size', '1'])).stderr, /next page: --page-token 1/);
  });

  it('artifact asks the server for the newest version of a kind, or a given one, escaping both', async () => {
    assert.equal((await cli(['artifact', 'ui:a', 'plan'])).stdout, 'new plan');
    assert.deepEqual(calls.at(-1)!.query, { q: 'kind:plan', pageSize: '1' });
    assert.equal((await cli(['artifact', 'ui:a', 'plan', '--version', '1'])).stdout, 'old plan');
    assert.deepEqual(calls.at(-1)!.query, { q: 'kind:plan AND version:1', pageSize: '1' });
    assert.deepEqual(JSON.parse((await cli(['artifact', 'ui:a', 'plan', '--json'])).stdout), sealed[0]);
    assert.equal(await qOf(['artifact', 'ui:a', 'sleeper.cards', '--version', '1 OR 2']), String.raw`kind:sleeper\.cards AND version:1\ OR\ 2`);
    assert.equal((await cli(['artifact', 'ui:a', 'nope'])).code, 2);
  });

  it('agents lists definitions', async () => {
    assert.match((await cli(['agents'])).stdout, /chat\s+3/);
  });

  it('sends QUACK_ADMIN_TOKEN when set and honors --url', async () => {
    await cli(['agents', '--url', url], { QUACK_ADMIN_TOKEN: 'tok', QUACK_URL: 'http://127.0.0.1:1' });
    assert.equal(calls[0]!.auth, 'Bearer tok');
  });

  it('gives up on a socket that never answers', async () => {
    const mute = createServer();
    mute.on('upgrade', () => {});
    await new Promise<void>((r) => mute.listen(0, '127.0.0.1', r));
    const c = client(`http://127.0.0.1:${(mute.address() as AddressInfo).port}`, undefined, 50);
    await assert.rejects(c.send('ui:a', 'x'), /cannot reach/);
    mute.closeAllConnections();
    mute.close();
  });

  it('an invoke retried after a dropped connection reuses its request id, and two invokes never share one', async () => {
    const ids: string[] = [];
    const flaky = createServer(async (req, res) => {
      ids.push((await readBody(req)).requestId);
      if (ids.length === 1) return res.destroy();
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ status: 'done' }));
    });
    await new Promise<void>((r) => flaky.listen(0, '127.0.0.1', r));
    const c = client(`http://127.0.0.1:${(flaky.address() as AddressInfo).port}`);
    assert.equal((await c.invoke('ui:a', 'hi', { wait: true })).status, 'done');
    await Promise.all([c.invoke('ui:a', 'x'), c.invoke('ui:a', 'y')]);
    flaky.close();
    assert.equal(ids[0], ids[1]);
    assert.equal(new Set(ids).size, 3);
  });

  it('exits 1 on usage errors and an unreachable server', async () => {
    assert.equal((await cli(['frobnicate'])).code, 1);
    assert.equal((await cli(['show'])).code, 1);
    const r = await cli(['agents'], { QUACK_URL: 'http://127.0.0.1:1' });
    assert.equal(r.code, 1);
    assert.match(r.stderr, /cannot reach/);
  });
});

describe('quack server', () => {
  const config = () => join(xdg, 'quack', 'config.json');
  const saveConfig = async (body: string) => { await mkdir(join(xdg, 'quack'), { recursive: true }); await writeFile(config(), body); };
  // QUACK_URL empty, so the saved servers decide.
  const bare = { QUACK_URL: '' };
  const serverNow = async (args: string[], env: Record<string, string> = bare) => (await cli([...args, '--help'], env)).stdout.split('\n').at(-1);

  it('picks --url, then --server, then $QUACK_URL, then the current saved server, then localhost', async () => {
    assert.equal(await serverNow([]), 'Server now: default http://localhost:8080');
    await saveConfig(JSON.stringify({ current: 'a', servers: { a: { url: 'http://a' }, b: { url: 'http://b' } } }));
    assert.equal(await serverNow([]), 'Server now: a http://a');
    assert.equal(await serverNow([], { QUACK_URL: 'http://env' }), 'Server now: $QUACK_URL http://env');
    assert.equal(await serverNow(['--server', 'b'], { QUACK_URL: 'http://env' }), 'Server now: b http://b');
    assert.equal(await serverNow(['--server', 'b', '--url', 'http://flag']), 'Server now: --url http://flag');
    const r = await cli(['agents', '--server', 'nope'], bare);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /no saved server nope/);
  });

  it('sends a saved server\'s token, and $QUACK_ADMIN_TOKEN for one saved without', async () => {
    await saveConfig(JSON.stringify({ current: 'tok', servers: { tok: { url, token: 'saved' }, plain: { url } } }));
    await cli(['agents'], { ...bare, QUACK_ADMIN_TOKEN: 'env' });
    await cli(['agents', '--server', 'plain'], { ...bare, QUACK_ADMIN_TOKEN: 'env' });
    assert.deepEqual(calls.map((c) => c.auth), ['Bearer saved', 'Bearer env']);
  });

  it('add checks /version and saves 0600, use makes it current, list marks it, remove forgets it', async () => {
    const added = await cli(['server', 'add', 'prod', `${url}/`, '--token', 'secret'], bare);
    assert.equal(added.code, 0, added.stderr);
    assert.equal(added.stdout, `saved prod: ${url}, version abcdef123456 built t`);
    assert.equal((await stat(config())).mode & 0o777, 0o600);
    assert.deepEqual(JSON.parse(await readFile(config(), 'utf8')), { servers: { prod: { url, token: 'secret' } } });
    await chmod(config(), 0o644);
    assert.equal((await cli(['server', 'use', 'prod'], bare)).stdout, `current server: prod ${url}`);
    assert.equal((await stat(config())).mode & 0o777, 0o600);
    assert.match((await cli(['server', 'list'], bare)).stdout, new RegExp(`^\\s+NAME\\s+URL\\s+TOKEN\\n\\*\\s+prod\\s+${url}\\s+saved$`));
    assert.doesNotMatch((await cli(['server', 'list', '--json'], bare)).stdout, /secret/);
    assert.equal((await cli(['server', 'list', '--keys'], bare)).stdout, 'prod');
    calls = [];
    assert.equal((await cli(['sessions', '--page-size', '3'], bare)).code, 0);
    assert.deepEqual([calls[0]!.path, calls[0]!.auth], ['/sessions', 'Bearer secret']);
    assert.equal((await cli(['server'], bare)).stdout, `prod  ${url}  reachable, version abcdef123456 built t`);
    assert.equal((await cli(['server', 'remove', 'prod'], bare)).code, 0);
    assert.deepEqual(JSON.parse(await readFile(config(), 'utf8')), { servers: {} });
    assert.equal((await cli(['server', 'use', 'prod'], bare)).code, 2);
  });

  it('add refuses a server that is not a distributor, an unreachable one and a bad URL, saving nothing', async () => {
    assert.equal((await cli(['server', 'add', 'x', `${url}/apps/nope`], bare)).code, 2);
    const other = await cli(['server', 'add', 'x', `${url}/elsewhere`], bare);
    assert.match(other.stderr, /not as a quack distributor/);
    assert.match((await cli(['server', 'add', 'x', 'http://127.0.0.1:1'], bare)).stderr, /cannot reach/);
    assert.match((await cli(['server', 'add', 'x', 'ftp://h'], bare)).stderr, /not an http or https URL/);
    assert.equal((await cli(['server', 'add', '__proto__', url], bare)).code, 1);
    await assert.rejects(stat(config()), { code: 'ENOENT' });
  });

  it('a corrupt config warns and falls back to localhost, and is never overwritten', async () => {
    await saveConfig('{not json');
    const help = await cli(['--help'], bare);
    assert.equal(help.code, 0);
    assert.match(help.stderr, /config\.json is not a quack config; fix or delete it; using http:\/\/localhost:8080/);
    assert.match(help.stdout, /Server now: default http:\/\/localhost:8080$/);
    const list = await cli(['server', 'list'], bare);
    assert.equal(list.code, 1);
    assert.doesNotMatch(list.stderr, /\n\s+at /);
    assert.equal((await cli(['server', 'add', 'prod', url], bare)).code, 1);
    assert.equal(await readFile(config(), 'utf8'), '{not json');
    await saveConfig(JSON.stringify({ servers: { a: { token: 1 } } }));
    assert.match((await cli(['server', 'list'], bare)).stderr, /not a quack config/);
  });
});

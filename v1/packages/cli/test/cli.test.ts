import assert from 'node:assert/strict';
import { createServer, type IncomingMessage, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, before, beforeEach, describe, it } from 'node:test';
import { WebSocketServer } from 'ws';
import { client } from '../src/api.ts';
import { run } from '../src/main.ts';

const sessions = [
  { key: 'ui:a', agent: 'chat', title: 'Alpha', running: true, active: true, createdAt: 't', lastActivity: 't' },
  { key: 'ui:b', agent: 'review', running: false, active: false, createdAt: 't', lastActivity: 't' },
  { key: 'ui:c', agent: 'chat', running: false, active: false, createdAt: 't', lastActivity: 't' },
];
const sealed = [
  { kind: 'plan', version: 1, content: 'old plan', verdict: null, summary: 's1' },
  { kind: 'plan', version: 2, content: 'new plan', verdict: { passed: true }, summary: 's2' },
];
const index: Record<string, unknown> = {
  approvals: [{ session: 'ui:b', id: 'ap1', tool: 'bash', at: 0 }],
  questions: [],
  failures: [{ session: 'ui:c', at: 0, step: 's', error: 'e' }],
  artifacts: [{ session: 'ui:a', kind: 'plan', version: 2, verdict: null, sealedAt: 0 }],
};

let server: Server;
let url: string;
let calls: Array<{ method: string; path: string; body: unknown; auth?: string }>;
let frames: unknown[];
let indexOff = false;

const readBody = async (req: IncomingMessage) => { let s = ''; for await (const c of req) s += c; return s ? JSON.parse(s) : undefined; };

before(async () => {
  server = createServer(async (req, res) => {
    const path = decodeURIComponent(req.url!);
    calls.push({ method: req.method!, path, body: await readBody(req), auth: req.headers.authorization });
    const reply = (status: number, body: object) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify(body)); };
    const idx = /^\/index\/(\w+)/.exec(path);
    if (path === '/sessions') return reply(200, { sessions });
    if (path === '/agents') return reply(200, { agents: [{ name: 'chat', version: 3 }] });
    if (idx) return indexOff ? reply(404, { error: 'not found' }) : reply(200, { [idx[1]!]: index[idx[1]!] });
    if (path.endsWith('/artifacts')) return reply(200, { artifacts: sealed });
    if (path.endsWith('/questions')) return reply(200, { questions: [{ id: 'q1', question: 'Which?' }] });
    if (path.endsWith('/approvals')) return reply(200, { approvals: [{ id: 'ap1', tool: 'bash' }] });
    if (path.endsWith('/approvals/ap9')) return reply(409, { error: 'unknown or already decided' });
    if (path.endsWith('/delete') && path.includes('ui:b')) return reply(409, { error: 'session is running' });
    reply(200, { key: 'ok' });
  });
  const wss = new WebSocketServer({ server });
  wss.on('connection', (ws) => {
    ws.send(JSON.stringify({ type: 'snapshot', entries: [{ model: [{ role: 'user', content: 'hello' }, { role: 'assistant', content: [{ type: 'text', text: 'hi there' }] }] }] }));
    ws.on('message', (d) => {
      frames.push(JSON.parse(String(d)));
      ws.send(JSON.stringify(JSON.parse(String(d)).input === 'bad' ? { type: 'error', error: 'rejected' } : { type: 'accepted' }));
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});
after(() => server.close());
beforeEach(() => { calls = []; frames = []; });

async function cli(args: string[], env: Record<string, string> = {}) {
  let stdout = '';
  let stderr = '';
  const code = await run(args, { QUACK_URL: url, ...env }, { stdout: (s) => (stdout += s), stderr: (s) => (stderr += s) });
  return { code, stdout, stderr };
}

describe('quack', () => {
  it('lists commands for --help', async () => {
    const r = await cli(['--help']);
    assert.equal(r.code, 0);
    for (const c of ['sessions', 'show', 'send', 'stop', 'resume', 'rename', 'archive', 'delete', 'approvals', 'approve', 'reject', 'artifacts', 'artifact', 'agents']) assert.match(r.stdout, new RegExp(`quack ${c}\\b`));
  });

  it('sessions prints the API JSON unchanged with --json and filters by agent', async () => {
    const r = await cli(['sessions', '--json', '--agent', 'chat']);
    assert.deepEqual(JSON.parse(r.stdout), { sessions: sessions.filter((s) => s.agent === 'chat') });
    assert.match((await cli(['sessions'])).stdout, /ui:a\s+chat\s+running\s+Alpha/);
  });

  it('sessions --state uses the index like the UI groups', async () => {
    const keys = async (state: string) => JSON.parse((await cli(['sessions', '--state', state, '--json'])).stdout).sessions.map((s: { key: string }) => s.key);
    assert.deepEqual(await keys('running'), ['ui:a']);
    assert.deepEqual(await keys('needs_you'), ['ui:b', 'ui:c']);
    assert.deepEqual(await keys('failed'), ['ui:c']);
    assert.deepEqual(await keys('done'), []);
    assert.equal((await cli(['sessions', '--state', 'nope'])).code, 1);
  });

  it('says so when the index is off', async () => {
    indexOff = true;
    const r = await cli(['approvals']);
    indexOff = false;
    assert.equal(r.code, 1);
    assert.match(r.stderr, /index is off/);
  });

  it('show gathers state, artifacts, questions, approvals and last messages', async () => {
    const r = await cli(['show', 'ui:a']);
    assert.equal(r.code, 0);
    assert.match(r.stdout, /plan\s+v2\s+passed\s+s2/);
    assert.match(r.stdout, /q1\s+Which\?/);
    assert.match(r.stdout, /ap1\s+bash/);
    assert.match(r.stdout, /assistant: hi there/);
    const j = JSON.parse((await cli(['show', 'ui:a', '--json'])).stdout);
    assert.deepEqual(j.artifacts, sealed);
    assert.deepEqual(j.messages, [{ role: 'user', text: 'hello' }, { role: 'assistant', text: 'hi there' }]);
  });

  it('exits 2 for an unknown session without touching it', async () => {
    const r = await cli(['show', 'ui:nope']);
    assert.equal(r.code, 2);
    assert.match(r.stderr, /no such session/);
    assert.deepEqual(calls.map((c) => c.path), ['/sessions']);
  });

  it('send returns once the server accepts, and reports a rejection', async () => {
    assert.equal((await cli(['send', 'ui:a', 'do', 'it'])).code, 0);
    assert.deepEqual(frames, [{ type: 'input', input: 'do it' }]);
    const bad = await cli(['send', 'ui:a', 'bad']);
    assert.equal(bad.code, 1);
    assert.match(bad.stderr, /rejected/);
  });

  it('stop, resume and housekeeping call the UI routes', async () => {
    await cli(['stop', 'ui:a']);
    await cli(['resume', 'ui:a']);
    await cli(['rename', 'ui:a', 'New', 'name']);
    await cli(['archive', 'ui:a']);
    await cli(['delete', 'ui:a']);
    const writes = calls.filter((c) => c.method !== 'GET').map((c) => [c.method, c.path, c.body]);
    assert.deepEqual(writes, [
      ['POST', '/sessions/ui:a/abort', undefined],
      ['POST', '/sessions/ui:a/resume', undefined],
      ['PATCH', '/sessions/ui:a', { title: 'New name' }],
      ['PATCH', '/sessions/ui:a', { archived: true }],
      ['POST', '/sessions/ui:a/delete', {}],
    ]);
  });

  it('turns an API refusal into exit 1 with its message', async () => {
    const r = await cli(['delete', 'ui:b']);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /session is running/);
  });

  it('approvals lists the inbox; approve and reject post decisions', async () => {
    assert.match((await cli(['approvals'])).stdout, /ui:b\s+ap1\s+bash/);
    assert.deepEqual(JSON.parse((await cli(['approvals', '--json'])).stdout), { approvals: index.approvals });
    await cli(['approve', 'ui:b', 'ap1']);
    await cli(['reject', 'ui:b', 'ap1', 'too', 'risky']);
    assert.deepEqual(calls.filter((c) => c.method === 'POST').map((c) => c.body), [{ decision: 'approve' }, { decision: 'reject', reason: 'too risky' }]);
    const stale = await cli(['approve', 'ui:b', 'ap9']);
    assert.equal(stale.code, 1);
    assert.match(stale.stderr, /unknown or already decided/);
    calls = [];
    for (const verb of ['approve', 'reject']) assert.equal((await cli([verb, 'ui:typo', 'ap1'])).code, 2);
    assert.deepEqual(calls.map((c) => c.method), ['GET', 'GET']);
  });

  it('artifacts lists and artifact prints the newest or a given version', async () => {
    assert.match((await cli(['artifacts'])).stdout, /ui:a\s+plan\s+v2/);
    assert.match((await cli(['artifacts', 'ui:a'])).stdout, /plan\s+v1\s+s1/);
    assert.equal((await cli(['artifact', 'ui:a', 'plan'])).stdout, 'new plan');
    assert.equal((await cli(['artifact', 'ui:a', 'plan', '--version', '1'])).stdout, 'old plan');
    assert.deepEqual(JSON.parse((await cli(['artifact', 'ui:a', 'plan', '--json'])).stdout), sealed[1]);
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
    await assert.rejects(c.snapshot('ui:a'), /cannot reach/);
    mute.closeAllConnections();
    mute.close();
  });

  it('exits 1 on usage errors and an unreachable server', async () => {
    assert.equal((await cli(['frobnicate'])).code, 1);
    assert.equal((await cli(['show'])).code, 1);
    const r = await cli(['agents'], { QUACK_URL: 'http://127.0.0.1:1' });
    assert.equal(r.code, 1);
    assert.match(r.stderr, /cannot reach/);
  });
});

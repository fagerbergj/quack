import assert from 'node:assert/strict';
import { mkdtemp, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
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
  approvals: [{ session: 'ui:b', id: 'ap1', tool: 'bash', at: '1970-01-01T00:00:00.000Z' }],
  questions: [],
  failures: [{ session: 'ui:c', at: '1970-01-01T00:00:00.000Z', step: 's', error: 'e' }],
  artifacts: [{ session: 'ui:a', kind: 'plan', version: 2, verdict: null, sealedAt: '1970-01-01T00:00:00.000Z' }],
};

let server: Server;
let url: string;
let calls: Array<{ method: string; path: string; body: unknown; auth?: string }>;
let frames: unknown[];
let indexOff = false;
let streaming = false;
let paged = false;

const readBody = async (req: IncomingMessage) => { let s = ''; for await (const c of req) s += c; return s ? JSON.parse(s) : undefined; };

before(async () => {
  server = createServer(async (req, res) => {
    const path = decodeURIComponent(req.url!);
    calls.push({ method: req.method!, path, body: await readBody(req), auth: req.headers.authorization });
    const reply = (status: number, body: object) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify(body)); };
    if (path.includes('/invocations')) {
      const body = calls.at(-1)!.body as { input: string; wait?: boolean };
      return reply(body.wait ? 200 : 202, body.input === 'fail' ? { status: 'unanswered', reason: 'model_fault' } : body.wait ? { status: 'done', answer: 'final answer' } : { status: 'running' });
    }
    const idx = /^\/index\/(\w+)/.exec(path);
    if (idx && paged) return reply(200, { [idx[1]!]: (index[idx[1]!] as unknown[]).slice(...(path.includes('pageToken=P2') ? [1] : [0, 1])), ...(!path.includes('pageToken=') && { nextPageToken: 'P2' }) });
    if (path === '/sessions') return reply(200, { sessions });
    if (path === '/agents') return reply(200, { agents: [{ name: 'chat', version: 3 }] });
    if (idx) return indexOff ? reply(404, { error: 'not found' }) : reply(200, { [idx[1]!]: index[idx[1]!] });
    if (path.endsWith('/export')) return reply(200, { session: sessions[0], transcript: [], artifacts: sealed });
    if (path.endsWith('/artifacts')) return reply(200, { artifacts: sealed });
    if (path.endsWith('/questions')) return reply(200, { questions: [{ id: 'q1', question: 'Which?' }] });
    if (path.endsWith('/approvals')) return reply(200, { approvals: [{ id: 'ap1', tool: 'bash' }] });
    if (path.endsWith('/approvals/ap9')) { res.writeHead(409, { 'content-type': 'application/problem+json' }); return res.end(JSON.stringify({ title: 'Conflict', status: 409, detail: 'unknown or already decided' })); }
    if (path.endsWith('/delete') && path.includes('ui:b')) return reply(409, { error: 'session is running' });
    reply(200, { key: 'ok' });
  });
  const wss = new WebSocketServer({ server });
  wss.on('connection', (ws) => {
    if (streaming) {
      const entry = (role: string, text: string) => ({ kind: 'entry', entry: { model: [{ role, content: [{ type: 'text', text }] }] } });
      ws.send(JSON.stringify({ type: 'view', items: [entry('user', 'go')], live: { run: {} } }));
      setTimeout(() => ws.send(JSON.stringify({ type: 'view', items: [entry('user', 'go'), entry('assistant', 'streamed one')], live: { run: {} } })), 20);
      setTimeout(() => ws.send(JSON.stringify({ type: 'view', live: {} })), 40);
      return;
    }
    ws.send(JSON.stringify({ type: 'view', items: [{ kind: 'earlier' }, { kind: 'entry', entry: { model: [{ role: 'user', content: 'hello' }, { role: 'assistant', content: [{ type: 'text', text: 'hi there' }] }] } }], live: {} }));
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
    for (const c of ['sessions', 'show', 'new', 'send', 'stop', 'resume', 'rename', 'archive', 'unarchive', 'delete', 'export', 'approvals', 'approve', 'reject', 'artifacts', 'artifact', 'agents', 'completion', 'eval']) assert.match(r.stdout, new RegExp(`quack ${c}\\b`));
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

  it('follows nextPageToken so the list is whole', async () => {
    const all = index.approvals;
    index.approvals = [{ session: 'ui:b', id: 'ap1', tool: 'bash', at: 'a' }, { session: 'ui:b', id: 'ap2', tool: 'bash', at: 'b' }];
    paged = true;
    try {
      const out = JSON.parse((await cli(['approvals', '--json'])).stdout) as { approvals: Array<{ id: string }> };
      assert.deepEqual(out.approvals.map((a) => a.id), ['ap1', 'ap2']);
      assert.ok(calls.some((c) => c.path === '/index/approvals?state=pending&pageToken=P2'));
    } finally {
      paged = false;
      index.approvals = all;
    }
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

  it('new starts a session through invocations and prints its key', async () => {
    const r = await cli(['new', '--agent', 'chat', 'plan', 'it']);
    assert.equal(r.code, 0);
    assert.match(r.stdout, /^ui:[0-9a-f-]{36}$/);
    const post = calls.find((c) => c.method === 'POST')!;
    assert.equal(post.path, `/sessions/${r.stdout}/invocations?agent=chat`);
    assert.equal((post.body as { input: string }).input, 'plan it');
    assert.equal((await cli(['new'])).code, 1);
  });

  it('-p waits for the run and prints only the answer, failing non-zero when it does not finish', async () => {
    const n = await cli(['new', '-p', '--agent', 'chat', 'hi']);
    assert.deepEqual([n.code, n.stdout], [0, 'final answer']);
    assert.match(n.stderr, /^session ui:/);
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
      assert.match(r.stdout, /user: go/);
      assert.match(r.stdout, /assistant: streamed one$/);
    } finally {
      streaming = false;
    }
  });

  it('unarchive clears the archived flag', async () => {
    await cli(['unarchive', 'ui:a']);
    assert.deepEqual(calls.filter((c) => c.method === 'PATCH').map((c) => c.body), [{ archived: false }]);
  });

  it('sessions --keys prints one key per line', async () => {
    assert.equal((await cli(['sessions', '--keys'])).stdout, 'ui:a\nui:b\nui:c');
  });

  it('completion prints a static script and rejects other shells', async () => {
    const bash = (await cli(['completion', 'bash'])).stdout;
    assert.match(bash, /complete -F _quack quack/);
    assert.match(bash, /compgen -W "sessions show new send /);
    assert.match(bash, /show\|/);
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
    await assert.rejects(c.view('ui:a'), /cannot reach/);
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

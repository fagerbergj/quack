import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { inProcessFetch, type LoopEvent } from '@quack/sdk';
import { homeApp } from '../src/notices.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const ended = (id: string, outcome: 'succeeded' | 'failed', failedSteps: Array<{ step: string; error: string }> = []): LoopEvent =>
  ({ seq: 9, id, at: 0, type: 'run.ended', data: { run: 1, outcome, failedSteps } });

// The distributor as Home's app reaches it in process: one session row and its newest artifact, and the invocations it is sent.
async function home(dbPath?: string) {
  const path = dbPath ?? join(await mkdtemp(join(tmpdir(), 'quack-home-app-')), 'home.sqlite');
  if (!dbPath) dirs.push(join(path, '..'));
  const sent: Array<{ url: string; body: Record<string, unknown> }> = [];
  const fetch: typeof globalThis.fetch = async (input, init) => {
    const url = String(input);
    if (init?.method === 'POST') return sent.push({ url, body: JSON.parse(String(init.body)) }), Response.json({ submissionId: '1', status: 'placed' });
    if (url.includes('/artifacts')) return Response.json({ artifacts: [{ kind: 'sleeper.waivers', version: 2, summary: 'Add Puka Nacua, drop a kicker' }] });
    return Response.json({ sessions: [{ title: 'Waivers week 9' }] });
  };
  let listener: ((session: string, event: LoopEvent) => void) | undefined;
  const app = homeApp({ dbPath: path, distributorUrl: 'http://d', fetch, subscribe: (on) => ((listener = on), () => (listener = undefined)) });
  const call = inProcessFetch((req, res) => app.handle(req, res));
  const started = (key: string) => call('http://app/started', { method: 'POST', body: JSON.stringify({ key }) });
  return { app, sent, started, path, hear: (session: string, e: LoopEvent) => listener!(session, e) };
}

test("a Home-started session's run end sends home:main one follow-up with the outcome, the newest result's summary and the link", async () => {
  const h = await home();
  assert.equal((await h.started('sleeper-waivers:w9')).status, 200);
  h.hear('sleeper-waivers:w9', ended('e1', 'succeeded'));
  h.hear('sleeper-waivers:w9', ended('e1', 'succeeded'));
  await h.app.stop!();
  assert.equal(h.sent.length, 1, 'a redelivered event is one notice');
  const [{ url, body }] = h.sent;
  assert.equal(url, 'http://d/sessions/home%3Amain/invocations?agent=quack');
  assert.equal(body.whenBusy, 'followUp');
  assert.match(String(body.input), /Waivers week 9 \(key sleeper-waivers:w9\), succeeded/);
  assert.match(String(body.input), /sleeper\.waivers v2: Add Puka Nacua, drop a kicker/);
  assert.match(String(body.input), /#\/s\/sleeper-waivers%3Aw9/);
});

test('a session Home did not start sends nothing, and other event types are ignored', async () => {
  const h = await home();
  await h.started('research:a');
  h.hear('github:fagerbergj/quack#1', ended('e1', 'succeeded'));
  h.hear('research:a', { seq: 1, id: 'r', at: 0, type: 'run.started', data: { run: 1 } });
  await h.app.stop!();
  assert.deepEqual(h.sent, []);
});

test('a failed run says failed and where', async () => {
  const h = await home();
  await h.started('research:b');
  h.hear('research:b', ended('e2', 'failed', [{ step: 'search', error: 'model unreachable' }]));
  await h.app.stop!();
  assert.match(String(h.sent[0]?.body.input), /, failed\.\nFailed at search: model unreachable/);
});

test('a notice already sent is not sent again after a restart redelivers its event', async () => {
  const first = await home();
  await first.started('research:c');
  first.hear('research:c', ended('e3', 'succeeded'));
  await first.app.stop!();
  const second = await home(first.path);
  second.hear('research:c', ended('e3', 'succeeded'));
  await second.app.stop!();
  assert.equal(first.sent.length + second.sent.length, 1);
});

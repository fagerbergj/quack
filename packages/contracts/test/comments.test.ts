import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createCatalog, fileJournal, sessionsApi, sqliteCatalog, type CatalogEvent } from '@quack/catalog';
import { inProcessFetch, serveApp } from '@quack/sdk';
import { contractsApp } from '../src/app.ts';
import { manifest } from '../src/manifest.ts';
import { lastReviewBlock } from '../src/reason.ts';

const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

const KEY = 'contracts:c';
const doc = (summary: string) => ({ openapi: '3.1.0', paths: { '/x': { get: { summary } }, '/y': { get: { summary: 'Y' } } } });
const specOf = (version: number, summary = 'X', changes?: unknown[]) => JSON.stringify({
  kind: 'rest', title: 'Things', version, document: doc(summary), choices: [{ id: 'c1', pointer: '', decision: 'One', why: [] }],
  intent: { purpose: 'p', audience: 'a', consumers: [], nonGoals: [], guarantees: [{ id: 'g.one', text: 'Fact one' }] }, ...(changes && { changes }),
});

// The catalog behind the Sessions API, the app over it with a fake clock and counted ids, and a caller per principal.
async function setup() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-contract-comments-'));
  const store = sqliteCatalog(join(dir, 'c.sqlite'), { currency: 'USD' });
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  const api = sessionsApi(catalog, { deleteSession: async () => 'deleted', answer: async () => true });
  const fetch = inProcessFetch(async (req, res) => void ((await api(req, res, { kind: 'tailnet' })) || res.writeHead(404).end('{}')));
  const clock = { now: Date.parse('2026-11-17T15:00:00Z') };
  let n = 0;
  const app = contractsApp({ settings: {}, dbPath: join(dir, 'app.sqlite'), distributorUrl: 'http://distributor.local', fetch, peerFetch: () => fetch, deployment: {}, env: {}, subscribe: () => () => {} }, { now: () => clock.now, newId: (p) => `${p}${++n}` });
  const server = serveApp(app);
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  cleanups.push(async () => { await new Promise((r) => server.close(r)); await app.stop?.(); await store.close(); await rm(dir, { recursive: true, force: true }); });
  const url = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  const seqs = new Map<string, number>();
  const publish = (key: string, events: Array<[string, unknown, string?]>) => catalog.publish(key, events.map(([type, data, id]) => {
    const seq = (seqs.get(key) ?? 0) + 1;
    seqs.set(key, seq);
    return { seq, id: id ?? `${type}:${seq}`, at: seq * 1000, type, data } as CatalogEvent;
  }));
  const seal = (key: string, kind: string, version: number, content: string) => publish(key, [['artifact.sealed', { kind, version, hash: `h${version}`, content }, `artifact:${kind}:${version}`]]);
  const propose = (key: string, id: string, artifact: number) => publish(key, [['ask.opened', { id, type: 'approval', createdAt: 1, toolCall: { tool: 'propose_contract', args: { version: artifact, artifact } } }]]);
  await publish(KEY, [['session.created', { agent: 'contracts', createdAt: 0 }, `session:${KEY}`]]);
  await seal(KEY, 'contract.spec', 1, specOf(1));
  await seal(KEY, 'contract.spec', 2, specOf(2));
  await propose(KEY, 'p2', 2);
  const call = (as: string, method: string, path: string, body?: unknown) => {
    const headers: Record<string, string> = { 'content-type': 'application/json' };
    if (as === 'session') headers.authorization = 'Bearer launch';
    else if (as !== 'tailnet') headers['x-authentik-username'] = as;
    return globalThis.fetch(`${url}/contracts/${encodeURIComponent(KEY)}${path}`, { method, headers, ...(body !== undefined && { body: JSON.stringify(body) }) });
  };
  const json = async (res: Response | Promise<Response>, status: number) => { const r = await res; const text = await r.text(); assert.equal(r.status, status, text); return text ? JSON.parse(text) : undefined; };
  const comment = (as: string, ptr = '/paths/~1x/get', body = 'Say what X returns.', artifact = 2) => json(call(as, 'POST', '/comments', { artifact, anchor: { on: 'document', ptr }, body }), 201);
  return { catalog, url, clock, call, json, comment, seal, propose, publish };
}
type Stack = Awaited<ReturnType<typeof setup>>;
const list = async (s: Stack, as: string, query = '') => (await s.json(s.call(as, 'GET', `/comments${query}`), 200)) as { comments: Array<Record<string, any>>; caller: unknown; nextPageToken?: string };

test('every comment and review route is a page route the gateway admits, and a session is refused on each', async () => {
  const s = await setup();
  const pages = manifest.pages!.filter((p) => p.path !== manifest.pages![0]!.path);
  const k = `/contracts/${encodeURIComponent(KEY)}`;
  const admitted = [['GET', `${k}/comments`], ['POST', `${k}/comments`], ['PATCH', `${k}/comments/cmt_1`], ['DELETE', `${k}/comments/cmt_1`], ['POST', `${k}/comments/cmt_1:resolve`], ['POST', `${k}/reviews`], ['GET', `${k}/reviews/rev_1`], ['DELETE', `${k}/reviews/rev_1`]];
  for (const [method, path] of admitted) assert.ok(pages.some((p) => p.method === method && (p.path as RegExp).test(path!)), `${method} ${path}`);
  for (const [method, path] of admitted) assert.equal((await s.call('session', method!, path!.slice(k.length), method === 'GET' || method === 'DELETE' ? undefined : {})).status, 403, `${method} ${path}`);
  assert.equal((await s.call('ada', 'PUT', '/comments')).status, 404);
  for (const key of ['contracts:missing', 'review:x', '%E0']) {
    assert.equal((await globalThis.fetch(`${s.url}/contracts/${key === '%E0' ? key : encodeURIComponent(key)}/comments`)).status, 404, key);
  }
});

test('create: the app sets the quote, only the version up for approval takes comments, and bodies and anchors are checked', async () => {
  const s = await setup();
  const c = await s.comment('ada');
  assert.deepEqual(c, { id: 'cmt_1', artifact: 2, version: 2, anchor: { on: 'document', ptr: '/paths/~1x/get' }, quote: '{"summary":"X"}', body: 'Say what X returns.', author: { kind: 'person', id: 'ada' }, state: 'draft', createdAt: '2026-11-17T15:00:00.000Z', replies: [] });
  const post = (body: unknown) => s.call('ada', 'POST', '/comments', body);
  await s.json(post({ artifact: 1, anchor: { on: 'choice', id: 'c1' }, body: 'old' }), 409);
  for (const bad of [{ artifact: 2, anchor: { on: 'document', ptr: '/paths/~1z' }, body: 'x' }, { artifact: 2, anchor: { on: 'choice', id: 'c1' }, body: '' }, { artifact: 2, anchor: { on: 'choice', id: 'c1' }, body: 'x'.repeat(1001) },
    { artifact: 2, anchor: { on: 'document', ptr: `/${'x'.repeat(1000)}` }, body: 'x' }, { artifact: 'two', anchor: { on: 'choice', id: 'c1' }, body: 'x' }, { artifact: 2, anchor: { on: 'guarantee', id: 'g.one', text: 'Other words' }, body: 'x' }]) {
    await s.json(post(bad), 400);
  }
  assert.equal((await s.json(post({ artifact: 2, anchor: { on: 'guarantee', id: 'g.one', text: 'Fact one' }, body: '€'.repeat(1000) }), 201)).quote, 'Fact one', '1000 characters, not bytes');
  const raw = await globalThis.fetch(`${s.url}/contracts/${encodeURIComponent(KEY)}/comments`, { method: 'POST', body: 'not json' });
  assert.equal(raw.status, 400);
});

test('drafts are private to their author, every tailnet caller is one author, and caller names who the API takes you to be', async () => {
  const s = await setup();
  await s.comment('ada');
  const mine = await s.comment('tailnet');
  assert.deepEqual((await list(s, 'bob')).comments, []);
  assert.deepEqual((await list(s, 'bob')).caller, { kind: 'person', id: 'bob' });
  assert.deepEqual((await list(s, 'ada')).comments.map((c) => c.id), ['cmt_1']);
  assert.deepEqual((await list(s, 'tailnet')).caller, { kind: 'tailnet' });
  assert.deepEqual((await list(s, 'tailnet')).comments.map((c) => c.id), [mine.id], 'another tailnet caller sees it');
  await s.json(s.call('tailnet', 'PATCH', `/comments/${mine.id}`, { body: 'Edited by another tailnet caller.' }), 200);
  await s.json(s.call('bob', 'PATCH', '/comments/cmt_1', { body: 'Not mine.' }), 403);
  await s.json(s.call('bob', 'DELETE', '/comments/cmt_1'), 403);
  await s.json(s.call('ada', 'PATCH', '/comments/cmt_1', { body: '' }), 400);
  assert.equal((await s.json(s.call('ada', 'PATCH', '/comments/cmt_1', { body: 'Better.' }), 200)).body, 'Better.');
  await s.json(s.call('ada', 'DELETE', '/comments/cmt_1'), 204);
  await s.json(s.call('ada', 'DELETE', '/comments/cmt_1'), 404);
  await s.json(s.call('ada', 'PATCH', '/comments/cmt_nope', { body: 'x' }), 404);
});

test('a list pages by pageSize with the token bound to q, filters on artifact and state, and refuses what it cannot read', async () => {
  const s = await setup();
  for (let i = 0; i < 5; i++) { s.clock.now += 1000; await s.comment('ada', i % 2 ? '/paths/~1y/get' : '/paths/~1x/get', `Comment ${i}`); }
  const first = await list(s, 'ada', '?pageSize=2&q=state:draft');
  assert.deepEqual(first.comments.map((c) => c.body), ['Comment 0', 'Comment 1']);
  const second = await list(s, 'ada', `?pageSize=2&q=state:draft&pageToken=${encodeURIComponent(first.nextPageToken!)}`);
  const third = await list(s, 'ada', `?pageSize=2&q=state:draft&pageToken=${encodeURIComponent(second.nextPageToken!)}`);
  assert.deepEqual([...second.comments, ...third.comments].map((c) => c.body), ['Comment 2', 'Comment 3', 'Comment 4']);
  assert.equal(third.nextPageToken, undefined);
  assert.equal((await list(s, 'ada', '?q=artifact:2 -state:sent')).comments.length, 5);
  assert.equal((await list(s, 'ada', '?q=artifact:1')).comments.length, 0);
  for (const bad of [`?pageSize=2&q=artifact:2&pageToken=${encodeURIComponent(first.nextPageToken!)}`, '?pageSize=0', '?pageSize=1001', '?pageToken=junk', '?q=mode:review', '?q=state:open', '?q=artifact:two', '?q=(']) {
    await s.json(s.call('ada', 'GET', `/comments${bad}`), 400);
  }
});

const prepare = (s: Stack, as: string, comments: string[], note?: string) => s.call(as, 'POST', '/reviews', { comments, ...(note !== undefined && { note }) });

test('a review freezes drafts into the exact reason, holds them, and reads sent only once the ask is rejected with that reason', async () => {
  const s = await setup();
  const a = await s.comment('ada');
  const b = await s.json(s.call('ada', 'POST', '/comments', { artifact: 2, anchor: { on: 'choice', id: 'c1' }, body: 'Why one?' }), 201);
  s.clock.now += 60_000;
  const review = await s.json(prepare(s, 'ada', [b.id, a.id], 'Two places.'), 201);
  const block = { review: review.id, artifact: 2, comments: [{ id: b.id, anchor: b.anchor, body: b.body }, { id: a.id, anchor: a.anchor, body: a.body }] };
  assert.deepEqual(review, { id: 'rev_3', artifact: 2, askId: 'p2', note: 'Two places.', comments: [b.id, a.id], by: { kind: 'person', id: 'ada' }, preparedAt: '2026-11-17T15:01:00.000Z', state: 'prepared', reason: `Two places.\n\n\`\`\`quack-review\n${JSON.stringify(block)}\n\`\`\`` });
  assert.deepEqual(lastReviewBlock(review.reason)?.block, block, 'the block round-trips');
  const held = (await list(s, 'ada')).comments;
  assert.deepEqual(held.map((c) => [c.state, c.review]), [['draft', review.id], ['draft', review.id]]);
  await s.json(s.call('ada', 'PATCH', `/comments/${a.id}`, { body: 'x' }), 409);
  await s.json(s.call('ada', 'DELETE', `/comments/${a.id}`), 409);
  await s.json(prepare(s, 'ada', [a.id]), 409);
  await s.json(s.call('bob', 'GET', `/reviews/${review.id}`), 403);
  assert.equal((await s.json(s.call('ada', 'GET', `/reviews/${review.id}`), 200)).reason, review.reason, 'a page that failed between the steps reads it again');
  await s.catalog.resolve(KEY, 'p2', { by: { kind: 'person', id: 'ada' }, choice: 'reject', reason: review.reason });
  assert.equal((await s.json(s.call('ada', 'GET', `/reviews/${review.id}`), 200)).state, 'sent');
  const sent = (await list(s, 'bob')).comments;
  assert.deepEqual(sent.map((c) => [c.state, c.review, typeof c.sentAt]), [['sent', review.id, 'string'], ['sent', review.id, 'string']], 'sent comments are no longer private');
  await s.json(s.call('ada', 'DELETE', `/reviews/${review.id}`), 409);
  await s.json(s.call('ada', 'PATCH', `/comments/${a.id}`, { body: 'x' }), 409);
  await s.json(s.call('bob', `POST`, `/comments/${a.id}:resolve`), 403);
  const resolved = await s.json(s.call('ada', 'POST', `/comments/${a.id}:resolve`), 200);
  assert.deepEqual([resolved.state, resolved.resolvedAt], ['resolved', '2026-11-17T15:01:00.000Z']);
  s.clock.now += 1;
  assert.equal((await s.json(s.call('ada', 'POST', `/comments/${a.id}:resolve`), 200)).resolvedAt, '2026-11-17T15:01:00.000Z', 'it stays resolved');
});

for (const [how, settle] of [
  ['approved', (s: Stack) => s.catalog.resolve(KEY, 'p2', { by: { kind: 'tailnet' }, choice: 'approve' })],
  ['withdrawn', (s: Stack) => s.publish(KEY, [['ask.cancelled', { id: 'p2', run: 1 }]])],
  ['rejected with an edited reason', (s: Stack) => s.catalog.resolve(KEY, 'p2', { by: { kind: 'tailnet' }, choice: 'reject', reason: 'Edited by hand.' })],
  ['deleted while prepared', async (s: Stack) => { await s.json(s.call('ada', 'DELETE', '/reviews/rev_2'), 204); }],
] as const) {
  test(`a review ${how} is released and its comments return to draft`, async () => {
    const s = await setup();
    const a = await s.comment('ada');
    await s.json(prepare(s, 'ada', [a.id]), 201);
    await s.json(s.call('bob', 'DELETE', '/reviews/rev_2'), 403);
    await settle(s);
    assert.equal((await s.json(s.call('ada', 'GET', '/reviews/rev_2'), 200)).state, 'released');
    const [c] = (await list(s, 'ada')).comments;
    assert.deepEqual([c!.state, c!.review], ['draft', undefined]);
    await s.json(s.call('ada', 'DELETE', '/reviews/rev_2'), 409);
    await s.json(s.call('ada', 'POST', `/comments/${a.id}:resolve`), 409);
  });
}

test('prepare refuses ids that are not your drafts, too many, a bad note, and a reason over 60,000 bytes as JSON', async () => {
  const s = await setup();
  const mine = await s.comment('ada');
  const theirs = await s.comment('bob');
  for (const [ids, note] of [[[theirs.id], undefined], [['cmt_none'], undefined], [[], undefined], [[mine.id, mine.id], undefined], [[mine.id], 'n'.repeat(2001)], [[mine.id], 'see ```quack-review below']] as const) {
    await s.json(prepare(s, 'ada', [...ids], note), 400);
  }
  const quotes = [];
  for (let i = 0; i < 20; i++) quotes.push((await s.comment('carol', '/paths/~1x/get', '"'.repeat(1000))).id);
  await s.json(prepare(s, 'carol', [...quotes, mine.id]), 400);
  const refused = await s.json(prepare(s, 'carol', quotes), 400);
  assert.match(refused.detail, /over 60000 bytes as JSON/);
  const euros = [];
  for (let i = 0; i < 20; i++) euros.push((await s.comment('dan', '/paths/~1x/get', '€'.repeat(1000))).id);
  await s.json(prepare(s, 'dan', euros), 400);
  assert.equal((await s.json(prepare(s, 'dan', euros.slice(0, 19)), 201)).comments.length, 19, 'UTF-8 bytes, under the limit');
});

test('100 drafts per artifact per author, then a 400', async () => {
  const s = await setup();
  for (let i = 0; i < 100; i++) await s.comment('ada', '/paths/~1x/get', `d${i}`);
  await s.json(s.call('ada', 'POST', '/comments', { artifact: 2, anchor: { on: 'choice', id: 'c1' }, body: 'one more' }), 400);
  await s.comment('bob');
});

test('state is derived on read: answered by a reply from this session, addressed only when a later row names it and its text changed, and outdated', async () => {
  const s = await setup();
  const changed = await s.comment('ada', '/paths/~1x/get', 'X is vague.');
  const kept = await s.comment('ada', '/paths/~1y/get', 'Y is fine?');
  const gone = await s.json(s.call('ada', 'POST', '/comments', { artifact: 2, anchor: { on: 'guarantee', id: 'g.one', text: 'Fact one' }, body: 'Drop it.' }), 201);
  const draft = await s.comment('ada', '/paths/~1y/get', 'Never sent.');
  const review = await s.json(prepare(s, 'ada', [changed.id, kept.id, gone.id]), 201);
  await s.catalog.resolve(KEY, 'p2', { by: { kind: 'person', id: 'ada' }, choice: 'reject', reason: review.reason });
  await s.publish('contracts:other', [['session.created', { agent: 'contracts', createdAt: 0 }, 'session:contracts:other']]);
  await s.seal('contracts:other', 'contract.reply', 1, JSON.stringify({ comment: kept.id, text: 'From another session.' }));
  await s.seal(KEY, 'contract.reply', 1, JSON.stringify({ comment: draft.id, text: 'To a draft.' }));
  assert.deepEqual((await list(s, 'ada')).comments.map((c) => [c.state, c.replies.length]), [['sent', 0], ['sent', 0], ['sent', 0], ['draft', 0]]);
  await s.seal(KEY, 'contract.reply', 2, JSON.stringify({ comment: kept.id, text: 'Y stays as it is.' }));
  const rows = [{ choice: 'c1', was: 'a', now: 'b', why: 'c', addresses: [changed.id, kept.id] }];
  const v3 = JSON.parse(specOf(3, 'X, now said plainly', rows));
  v3.intent.guarantees = [];
  await s.seal(KEY, 'contract.spec', 3, JSON.stringify(v3));
  const [a, b, c, d] = (await list(s, 'ada')).comments;
  assert.deepEqual([a!.state, a!.addressedIn, a!.outdated], ['addressed', 3, { mark: 'changed', in: 3 }]);
  assert.deepEqual([b!.state, b!.replies.map((r: { text: string }) => r.text), b!.addressedIn, b!.outdated], ['answered', ['Y stays as it is.'], undefined, undefined], 'named while its text is unchanged: not addressed');
  assert.deepEqual([c!.state, c!.outdated], ['sent', { mark: 'removed', in: 3 }]);
  assert.deepEqual([d!.state, d!.replies], ['draft', []]);
  const v4 = JSON.parse(specOf(4, 'X, now said plainly'));
  v4.intent.guarantees = [{ id: 'g.one', text: 'Fact one, reworded' }];
  await s.seal(KEY, 'contract.spec', 4, JSON.stringify(v4));
  assert.deepEqual((await list(s, 'ada')).comments[2]!.outdated, { mark: 'changed', in: 3 }, 'back with other text: changed, first seen in 3');
});

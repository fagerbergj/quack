import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { defineExtension, defineTool, type Extension } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts } from '@quack/sdk';
import { findingItems } from '@quack/sdk/report';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { readSession, replayJudge } from '@quack/eval';
import { manifest as judgeManifest, type Inputs, type JudgeConfig } from '@quack/judge';
import { formatResults, htmlToText, isPublicAddress, manifest, reportInputs, researchSessionExtensions, safeGet, sourceText, SourcesDoc, validateReport } from '../src/index.ts';

const servers: Server[] = [];
const dirs: string[] = [];
after(async () => {
  servers.forEach((s) => s.close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});

async function listen(handler: Parameters<typeof createServer>[1]) {
  const server = createServer(handler).listen(0);
  servers.push(server);
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

const PDF = (text: string) => {
  const stream = `BT /F1 12 Tf 20 100 Td (${text}) Tj ET`;
  return `%PDF-1.1\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 300 200]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj\n4 0 obj<</Length ${stream.length}>>stream\n${stream}\nendstream endobj\n5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj\ntrailer<</Root 1 0 R/Size 6>>\n%%EOF\n`;
};

// The fake servers sit on loopback, so tests treat 127.0.0.1 as public and everything else by the real policy.
const loopbackOk = (ip: string) => ip === '127.0.0.1' || isPublicAddress(ip);

const web = () => listen((req, res) => {
  if (req.url === '/page') return res.writeHead(200, { 'content-type': 'text/html' }).end('<html><head><title>Ducks</title><script>var x=1</script></head><body><h1>Ducks</h1><p>Ducks &amp; geese fly.</p></body></html>');
  if (req.url === '/paper.pdf') return res.writeHead(200, { 'content-type': 'application/pdf' }).end(PDF('Mallards migrate'));
  if (req.url === '/huge') return res.writeHead(200, { 'content-type': 'text/plain' }).end('x'.repeat(600_000));
  if (req.url === '/long') return res.writeHead(200, { 'content-type': 'text/plain' }).end('abcdefghij'.repeat(10_000));
  if (req.url === '/big') return res.writeHead(200, { 'content-type': 'text/plain' }).end('x'.repeat(2000));
  if (req.url === '/hop') return res.writeHead(302, { location: `http://127.0.0.2:${(req.socket.localPort)}/page` }).end();
  if (req.url === '/samehop') return res.writeHead(302, { location: '/page' }).end();
  res.writeHead(404).end('gone');
});

const exa = (seen: unknown[]) => listen(async (req, res) => {
  let raw = '';
  for await (const c of req) raw += c;
  seen.push({ key: req.headers['x-api-key'], url: req.url, body: JSON.parse(raw) });
  res.writeHead(200).end(JSON.stringify({ results: [{ title: 'Duck facts', url: 'https://example.org/ducks', publishedDate: '2026-03-04T00:00:00.000Z', text: 'Ducks\n  are birds.' }, { url: 'https://example.org/bare' }] }));
});

test('addresses: private, loopback, link-local and wrapped forms are refused, public ones pass', () => {
  for (const a of ['127.0.0.1', '10.1.2.3', '172.20.0.1', '192.168.1.1', '169.254.169.254', '100.64.0.1', '0.0.0.0', '::1', '::', 'fe80::1', 'fd00::1', '::ffff:127.0.0.1', '::ffff:7f00:1', '::ffff:a00:1', '::127.0.0.1', '::7f00:1', '240.0.0.1', '255.255.255.255', '64:ff9b::7f00:1', 'not-an-ip']) assert.equal(isPublicAddress(a), false, a);
  for (const a of ['8.8.8.8', '93.184.216.34', '2606:4700:4700::1111', '::ffff:8.8.8.8', '::ffff:808:808']) assert.equal(isPublicAddress(a), true, a);
});

test('safeGet refuses loopback by address and by name, and non-http schemes, without connecting', async () => {
  let hits = 0;
  const base = await listen((_req, res) => (hits++, res.end('secret')));
  const o = { maxBytes: 1000, timeoutMs: 5000 };
  await assert.rejects(safeGet(`${base}/`, o), /not a public address/);
  await assert.rejects(safeGet(`http://localhost:${base.split(':')[2]}/`, o), /non-public address/);
  await assert.rejects(safeGet('http://[::1]:1/', o), /not a public address/);
  await assert.rejects(safeGet('file:///etc/passwd', o), /only http and https/);
  assert.equal(hits, 0);
});

test('safeGet follows redirects, caps size and reports status', async () => {
  const base = await web();
  const o = { maxBytes: 1000, timeoutMs: 5000, isPublic: loopbackOk };
  assert.equal((await safeGet(`${base}/samehop`, o)).url, `${base}/page`);
  await assert.rejects(safeGet(`${base}/big`, o), /larger than 1000 bytes/);
  assert.equal((await safeGet(`${base}/nope`, o)).status, 404);
});

test('safeGet re-checks every redirect hop: a hop to another loopback address is refused before connecting', async () => {
  const base = await web();
  await assert.rejects(safeGet(`${base}/hop`, { maxBytes: 1000, timeoutMs: 5000, isPublic: loopbackOk }), /127\.0\.0\.2 is not a public address/);
});

test('text extraction and result shaping', () => {
  assert.equal(htmlToText('<title>T &amp; U</title><style>p{}</style><p>One&nbsp;two</p><p>&#65;&lt;b&gt;</p>'), 'T & U\n\nOne two\nA<b>');
  assert.equal(formatResults([{ title: 'A', url: 'https://a.test/', publishedDate: '2026-01-02T03:04:05Z', text: 'x\n  y' }, { url: 'https://b.test/' }]), '1. A (2026-01-02)\n   https://a.test/\n   x y\n2. https://b.test/\n   https://b.test/');
  assert.equal(formatResults([]), 'No results.');
});

test('page text breaks at block boundaries and keeps inline-split words whole', async () => {
  const curl = htmlToText(await readFile(join(import.meta.dirname, 'fixtures/summary-quote.html'), 'utf8'));
  assert.match(curl, /^Quote\nA basic rule of thumb/m);
  assert.doesNotMatch(curl, /QuoteA/);
  assert.match(curl, /^A basic rule of thumb is that if someone can spot that the contribution was made\nwith the help of AI, you have more work to do\.$/m);
  assert.equal(htmlToText('<p>An <em>ex</em>ample of <a href="/x">li</a>nks</p>'), 'An example of links');
  assert.equal(htmlToText('<div>Label<p>One</p>\n\n<p>Two</p></div>'), 'Label\nOne\nTwo');
  assert.equal(htmlToText('<ul><li>a</li><li>b</li></ul>x<br>y<br/>z'), 'a\nb\nx\ny\nz');
  assert.equal(htmlToText('<table><tr><td>1</td><td>2</td></tr><tr><th>3</th></tr></table><pre>p</pre><path/>q'), '1 2\n3\np\nq');
});

test('a report must have its sections and cite a source', () => {
  assert.deepEqual(validateReport('## Summary\nx\n## Assumptions\n-\n## Findings\n- y [s1]\n'), []);
  assert.deepEqual(validateReport('## Summary\nx\n'), ['missing a "## Assumptions" section', 'missing a "## Findings" section', 'no source is cited; cite fetched sources as [s1], [s2]']);
});

const report = (cite: string) => `## Summary\nDucks fly.\n## Assumptions\nNone.\n## Findings\n- Ducks and geese fly. ${cite}\n`;
const call = (name: string, args: Record<string, unknown>) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const seeing = (seen: string[]) => (msg: ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), msg);

// The judge model finds no style problem, so only the link gate can fail a report here.
function judgeFaux() {
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  faux.setResponses(Array.from({ length: 200 }, () => () => fauxAssistantMessage(JSON.stringify({ choice: 'no', confidence: 1, reason: 'ok' }))));
  return faux;
}

async function run(config: Parameters<typeof researchSessionExtensions>[0] & JudgeConfig, steps: (step: ReturnType<typeof seeing>) => FauxResponseStep[], deps?: Parameters<typeof researchSessionExtensions>[1], extra: Extension[] = []) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses(steps(seeing(seen)));
  const models = buildModels(parseDeployment({ defaultModel: 'unused/m', providers: { unused: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } }));
  models.setProvider(faux.provider);
  models.setProvider(judgeFaux().provider);
  const m = faux.getModel();
  const dir = await mkdtemp(join(tmpdir(), 'quack-research-'));
  dirs.push(dir);
  const { maxRevisions, ...research } = config ?? {};
  // The judge installs first, as the agent's resolved order puts it before research.
  const judge = await judgeManifest.session!({ judgeModel: { provider: 'judge', modelId: 'j' }, maxRevisions }, {}, []);
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...judge, ...researchSessionExtensions(research, deps), ...extra] } });
  const done = await session.submit('Do ducks fly?', 'r-1', true);
  return { done, seen, session, dir };
}

test('a run searches, fetches HTML and PDF, records each fetch and search hit, and a report linking a page it never retrieved is sent back', async () => {
  const base = await web();
  const searches: any[] = [];
  process.env.QUACK_EXA_API_KEY = 'test-key';
  const { done, seen, session } = await run({ exaUrl: await exa(searches) }, (step) => [
    call('web_search', { queries: ['do ducks fly'], count: 2 }),
    step(call('web_fetch', { urls: [`${base}/page`] })),
    step(call('web_fetch', { urls: [`${base}/paper.pdf`] })),
    step(call('web_fetch', { urls: [`${base}/nope`] })),
    step(call('write', { path: 'artifacts/report.md', content: `${report('[s1]')}- Ducks migrate south, per <https://example.org/geese>.\n` })),
    step(call('submit_report', {})),
    step(call('write', { path: 'artifacts/report.md', content: `${report('[s1][s2]')}- Ducks are birds, per [Duck facts](https://example.org/ducks).\n` })),
    step(call('submit_report', {})),
    step(fauxAssistantMessage('Ducks fly.')),
  ], { isPublic: loopbackOk });
  assert.equal(done.status === 'done' && done.answer, 'Ducks fly.');
  assert.deepEqual(searches, [{ key: 'test-key', url: '/search', body: { query: 'do ducks fly', numResults: 2, contents: { text: { maxCharacters: 400 } } } }]);
  assert.match(seen[0], /1\. Duck facts \(2026-03-04\)\\n {3}https:\/\/example.org\/ducks\\n {3}Ducks are birds\./);
  assert.match(seen[1], /\[s1\] http:\/\/127.0.0.1:\d+\/page\\n\\nDucks\\n\\nDucks\\nDucks & geese fly\./);
  assert.match(seen[2], /\[s2\] .*paper\.pdf\\n\\nMallards migrate/);
  assert.match(seen[3], /answered 404/);
  assert.match(seen[5], /gate links\.links_retrieved: https:\/\/example\.org\/geese was neither fetched nor in a search result \(the nearest page this session retrieved is https:\/\/example\.org\/ducks\)/);

  const sources = await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT);
  assert.deepEqual(sources!.sources.map((s) => s.id), ['s1', 's2'], 'the 404 left no record');
  assert.match(sources!.sources[0].hash, /^[0-9a-f]{64}$/);
  assert.ok(sources!.sources[0].fetchedAt > 0);
  assert.deepEqual(sources!.found!.map((f) => f.url), ['https://example.org/ducks', 'https://example.org/bare'], 'each search hit, once');
  const kinds = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['research.report'];
  assert.deepEqual(kinds.versions.map((v) => (v.verdict as any).passed), [false, true]);
  await session.close();
});

test('a recorded session rebuilds, for each sealed report, the inputs the live judge read when it judged it', async () => {
  const base = await web();
  const live: Inputs[] = [];
  const builder = reportInputs();
  // The judge's own inputs are tapped, so the replay is compared with what the judge itself was given.
  const inputs: typeof builder = async (...a) => { const got = await builder(...a); live.push(got); return got; };
  const { session, dir } = await run({}, (step) => [
    call('web_fetch', { urls: [`${base}/page`] }),
    // Version 1 cites s2 before any fetch returned it, so the link gate fails it and the agent revises.
    step(call('write', { path: 'artifacts/report.md', content: `${report('[s1][s2]')}- Ducks migrate south.\n` })),
    step(call('submit_report', {})),
    // s2 is fetched only after version 1 was judged, so its evidence must not count it.
    step(call('web_fetch', { urls: [`${base}/paper.pdf`] })),
    step(call('write', { path: 'artifacts/report.md', content: `${report('[s1][s2]')}- Mallards migrate. [s2]\n` })),
    step(call('submit_report', {})),
    step(fauxAssistantMessage('Ducks fly.')),
  ], { isPublic: loopbackOk, inputs });
  await session.close();
  const record = await readSession(dir);
  const versions = record.artifacts['research.report'];
  assert.equal(versions.length, 2);
  const adapter = manifest.evalAdapters![0].judge!;
  const replayed = await Promise.all(versions.map((v) => adapter.inputs!(record, v.content)));
  assert.deepEqual(replayed, live);
  assert.deepEqual(live.map((i) => JSON.parse(i.log).fetched.map((f: { id: string }) => f.id)), [['s1'], ['s1', 's2']]);
  await assert.rejects(adapter.inputs!(record, 'never sealed'), /not a version this session sealed/);
});

test('without the Exa key web_search says so, and a private URL is refused by default', async () => {
  delete process.env.QUACK_EXA_API_KEY;
  const base = await web();
  const { seen, session } = await run({}, (step) => [
    call('web_search', { queries: ['x'] }),
    step(call('web_fetch', { urls: [`${base}/page`] })),
    step(fauxAssistantMessage('Stopped.')),
  ]);
  assert.match(seen[0], /QUACK_EXA_API_KEY is not set/);
  assert.match(seen[1], /not a public address/);
  const sources = await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT);
  assert.deepEqual(sources?.sources ?? [], []);
  await session.close();
});

const readSource = defineTool({
  name: 'source_text', description: 'test', parameters: obj({ id: str() }, { open: true }), replay: 'safe',
  execute: async (a, api, ctx) => ({ output: [{ type: 'text' as const, text: (await sourceText(api, a.id, ctx)) ?? 'none' }] }),
});
const sourceLength = defineTool({
  name: 'source_length', description: 'test', parameters: obj({ id: str() }, { open: true }), replay: 'safe',
  execute: async (a, api, ctx) => ({ output: [{ type: 'text' as const, text: String((await sourceText(api, a.id, ctx))?.length) }] }),
});
const readSourceExt = defineExtension({ name: 'test', tools: [readSource, sourceLength] });
const delivered = (seen: string[]) => JSON.parse(seen.at(-1)!).content[0].text as string;

test('a fetched source keeps its text, retrievable by id and keyed by the recorded hash', async () => {
  const base = await web();
  const { seen, session } = await run({ maxFetchBytes: 1000 }, (step) => [
    call('web_fetch', { urls: [`${base}/page`] }),
    step(call('source_text', { id: 's1' })),
    step(call('source_text', { id: 's2' })),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk }, [readSourceExt]);
  assert.match(delivered(seen.slice(0, 2)), /^Ducks\n\nDucks\nDucks & geese fly\.$/);
  assert.equal(delivered(seen.slice(0, 3)), 'none');
  const sources = (await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT))!.sources;
  assert.equal(sources.length, 1);
  await session.close();
});

test('stored source text is capped at 500k characters, still retrievable, and both tools say where the stored text stops', async () => {
  const base = await web();
  const { seen, session } = await run({ maxFetchBytes: 1_000_000 }, (step) => [
    call('web_fetch', { urls: [`${base}/huge`] }),
    step(call('source_length', { id: 's1' })),
    step(call('read_source', { id: 's1', offset: 480_000 })),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk }, [readSourceExt]);
  assert.match(sees(seen, 0), /cut at 30000 of 600000 characters; read the rest with read_source id "s1" offset 30000 \(only the first 500000 are kept\)$/);
  assert.match(delivered(seen.slice(0, 2)), /^500000$/);
  const tail = sees(seen, 2);
  assert.equal(tail.split('\n')[0].length, 20_000);
  assert.match(tail, /end of the stored text of source s1: only its first 500000 of 600000 characters were kept, so the rest cannot be read$/);
  assert.doesNotMatch(tail, /end of source/);
  await session.close();
});

test('read_source pages a stored 100k source to its end, names the offset on truncation, and rejects unknown ids', async () => {
  const base = await web();
  const { seen, session } = await run({}, (step) => [
    call('web_fetch', { urls: [`${base}/long`] }),
    step(call('read_source', { id: 's1', offset: 30_000 })),
    step(call('read_source', { id: '[s1]', offset: 60_000 })),
    step(call('read_source', { id: 's1', offset: 90_000 })),
    step(call('read_source', { id: 's9', offset: 0 })),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk });
  assert.match(sees(seen, 0), /… cut at 30000 of 100000 characters; read the rest with read_source id "s1" offset 30000$/);
  const w1 = sees(seen, 1);
  assert.match(w1, /^abcdefghij/);
  assert.match(w1, /… 60000 of 100000 characters; continue with read_source id "s1" offset 60000$/);
  assert.match(sees(seen, 2), /offset 90000$/);
  const last = sees(seen, 3);
  assert.equal(last.split('\n')[0].length, 10_000);
  assert.match(last, /end of source s1 \(100000 characters\)$/);
  assert.match(sees(seen, 4), /no stored text for source s9/);
  await session.close();
});

const sees = (seen: string[], n: number) => JSON.parse(seen[n]).content[0].text as string;

test('a batch of URLs runs together, reports each on its own, and ids follow input order', async () => {
  const base = await web();
  const { seen, session } = await run({}, (step) => [
    call('web_fetch', { urls: [`${base}/paper.pdf`, `${base}/nope`, `${base}/page`] }),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk });
  const out = sees(seen, 0);
  assert.match(out, /^\[s1\] .*paper\.pdf\n\nMallards migrate\n\n---\n\n.*answered 404\n\n---\n\n\[s2\] .*\/page\n\nDucks/);
  const sources = (await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT))!.sources;
  assert.deepEqual(sources.map((s) => s.id), ['s1', 's2']);
  await session.close();
});

test('a batched private URL and a redirect into a private address are refused per item while the rest are fetched', async () => {
  const base = await web();
  const port = base.split(':')[2];
  const { seen, session } = await run({}, (step) => [
    call('web_fetch', { urls: [`${base}/page`, `http://127.0.0.2:${port}/page`, `${base}/hop`, 'http://[::1]:1/'] }),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk });
  const out = sees(seen, 0);
  assert.match(out, /^\[s1\] .*\/page/);
  assert.match(out, /cannot fetch http:\/\/127\.0\.0\.2:\d+\/page: 127\.0\.0\.2 is not a public address/);
  assert.match(out, /cannot fetch .*\/hop: 127\.0\.0\.2 is not a public address/);
  assert.match(out, /cannot fetch http:\/\/\[::1\]:1\/: ::1 is not a public address/);
  assert.equal((await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT))!.sources.length, 1);
  await session.close();
});

test('a batch where every URL is refused is an error', async () => {
  const base = await web();
  const { seen, session } = await run({}, (step) => [call('web_fetch', { urls: [`${base}/page`, `${base}/hop`] }), step(fauxAssistantMessage('Done.'))]);
  assert.match(sees(seen, 0), /not a public address[\s\S]*not a public address/);
  assert.match(seen[0], /"isError":true/);
  await session.close();
});

test('a batch of searches reports each query, and one failing query does not fail the others', async () => {
  process.env.QUACK_EXA_API_KEY = 'test-key';
  const exaUrl = await listen(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    if (JSON.parse(raw).query === 'bad') return res.writeHead(500).end('boom');
    if (JSON.parse(raw).query === 'garbled') return res.writeHead(200).end('<html>not json');
    res.writeHead(200).end(JSON.stringify({ results: [{ title: 'Hit', url: 'https://example.org/hit' }] }));
  });
  const { seen, session } = await run({ exaUrl }, (step) => [call('web_search', { queries: ['good', 'bad', 'garbled', 'also good'] }), step(fauxAssistantMessage('Done.'))]);
  assert.match(sees(seen, 0), /^## good\n1\. Hit\n {3}https:\/\/example\.org\/hit\n\n## bad\nsearch failed: 500 boom\n\n## garbled\nsearch failed: .*JSON.*\n\n## also good\n1\. Hit/);
  await session.close();
});

const cited = (report: string) => findingItems(report).map((f) => [f.first, /\[s\d+\]/.test(f.text)]);

test('findingItems: only the "## Findings" section counts, not one that merely starts with the word', () => {
  const r = '## Summary\nx\n## Findings\n- Sorel, HIC 205379. [s9]\n## Findings — Raynham\n- Naylor, CSL 042837. [s15]\n## Shortlist\n- Sorel first.\n';
  assert.deepEqual(cited(r), [['- Sorel, HIC 205379. [s9]', true]]);
});

test('findingItems: a mark on the paragraph above a list or on the bullet a sub-bullet sits in cites it; a bullet that only introduces sub-bullets is not a finding', () => {
  const r = [
    '## Findings',
    'A-MAC scores each memory on factors: [s16]',
    '',
    '- Utility: an LLM rates future use.',
    '- Novelty: one minus the top cosine.',
    '',
    'FadeMem decays memories: [s6]',
    '```',
    'v = v0 * exp(-l * t)',
    '```',
    '- Pruning below a floor.',
    '',
    'The same scalar drives two operations:',
    '- Encoding depth: four tiers.',
    '- Forgetting: high value resists. [s3]',
    '',
    'Later paragraph [s21].',
    '- Covered by the paragraph above.',
    '',
    '### Packages',
    '- **pi-durable (intent):**',
    '  - replay is safe or unsafe [s6]',
    '  - callers may be the model',
    '- **Zalando** prefers problem+json. [s58]',
    '  - Azure uses its own envelope.',
    '- **Disagreement:** follow the house standard.',
  ].join('\n');
  assert.deepEqual(cited(r), [
    ['- Utility: an LLM rates future use.', true],
    ['- Novelty: one minus the top cosine.', true],
    ['- Pruning below a floor.', true],
    ['- Encoding depth: four tiers.', false],
    ['- Forgetting: high value resists. [s3]', true],
    ['- Covered by the paragraph above.', true],
    ['- replay is safe or unsafe [s6]', true],
    ['- callers may be the model', false],
    ['- **Zalando** prefers problem+json. [s58]', true],
    ['- Azure uses its own envelope.', true],
    ['- **Disagreement:** follow the house standard.', false],
  ]);
  assert.deepEqual(cited('## Findings\n- One.\n\nA paragraph after the list. [s2]\n'), [['- One.', false]], 'a later paragraph is not the bullet\'s');
  const texts = findingItems(r).map((f) => f.text);
  assert.equal(texts[0], 'A-MAC scores each memory on factors: [s16]\n- Utility: an LLM rates future use.', 'a marked lead-in is the claim\'s context');
  assert.equal(texts[3], '- Encoding depth: four tiers.', 'an unmarked lead-in is left out of the claim the judge reads');
});

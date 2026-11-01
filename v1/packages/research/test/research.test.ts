import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, Type, type FauxResponseStep } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, type Extension } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts } from '@quack/sdk';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { deliveredReport, findingItems, formatResults, htmlToText, isPublicAddress, researchRubric, researchSessionExtensions, safeGet, sourceText, SourcesDoc, validateReport } from '../src/index.ts';

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

test('a report must have its sections and cite a source', () => {
  assert.deepEqual(validateReport('## Summary\nx\n## Assumptions\n-\n## Findings\n- y [s1]\n'), []);
  assert.deepEqual(validateReport('## Summary\nx\n'), ['missing a "## Assumptions" section', 'missing a "## Findings" section', 'no source is cited; cite fetched sources as [s1], [s2]']);
});

const report = (cite: string) => `## Summary\nDucks fly.\n## Assumptions\nNone.\n## Findings\n- Ducks and geese fly. ${cite}\n`;
const call = (name: string, args: Record<string, unknown>) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const seeing = (seen: string[]) => (msg: ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), msg);

async function run(config: Parameters<typeof researchSessionExtensions>[0], steps: (step: ReturnType<typeof seeing>) => FauxResponseStep[], deps?: Parameters<typeof researchSessionExtensions>[1], extra: Extension[] = []) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses(steps(seeing(seen)));
  const models = buildModels(parseDeployment({ providers: { unused: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } }));
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const dir = await mkdtemp(join(tmpdir(), 'quack-research-'));
  dirs.push(dir);
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...researchSessionExtensions(config, deps), ...extra] } });
  const done = await session.submit('Do ducks fly?', 'r-1', true);
  return { done, seen, session };
}

test('a run searches, fetches HTML and PDF, records each fetch, and a report citing an unfetched source is sent back', async () => {
  const base = await web();
  const searches: any[] = [];
  process.env.QUACK_EXA_API_KEY = 'test-key';
  const { done, seen, session } = await run({ exaUrl: await exa(searches) }, (step) => [
    call('web_search', { queries: ['do ducks fly'], count: 2 }),
    step(call('web_fetch', { urls: [`${base}/page`] })),
    step(call('web_fetch', { urls: [`${base}/paper.pdf`] })),
    step(call('web_fetch', { urls: [`${base}/nope`] })),
    step(call('write', { path: 'artifacts/report.md', content: report('[s1][s3]') })),
    step(call('submit_report', {})),
    step(call('write', { path: 'artifacts/report.md', content: report('[s1][s2]') })),
    step(call('submit_report', {})),
    step(fauxAssistantMessage('Ducks fly.')),
  ], { isPublic: loopbackOk });
  assert.equal(done.status === 'done' && done.answer, 'Ducks fly.');
  assert.deepEqual(searches, [{ key: 'test-key', url: '/search', body: { query: 'do ducks fly', numResults: 2, contents: { text: { maxCharacters: 400 } } } }]);
  assert.match(seen[0], /1\. Duck facts \(2026-03-04\)\\n {3}https:\/\/example.org\/ducks\\n {3}Ducks are birds\./);
  assert.match(seen[1], /\[s1\] http:\/\/127.0.0.1:\d+\/page\\n\\nDucks\\n\\nDucks\\nDucks & geese fly\./);
  assert.match(seen[2], /\[s2\] .*paper\.pdf\\n\\nMallards migrate/);
  assert.match(seen[3], /answered 404/);
  assert.match(seen[5], /gate claims_grounded\.cites_fetched_sources: cites s3, which this session never fetched/);

  const sources = await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT);
  assert.deepEqual(sources!.sources.map((s) => s.id), ['s1', 's2'], 'the 404 left no record');
  assert.match(sources!.sources[0].hash, /^[0-9a-f]{64}$/);
  assert.ok(sources!.sources[0].fetchedAt > 0);
  const kinds = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['research.report'];
  assert.deepEqual(kinds.versions.map((v) => (v.verdict as any).passed), [false, true]);
  await session.close();
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
  name: 'read_source', description: 'test', parameters: Type.Object({ id: Type.String() }), replay: 'safe',
  execute: async (a, api, ctx) => ({ output: [{ type: 'text' as const, text: (await sourceText(api, a.id, ctx)) ?? 'none' }] }),
});
const sourceLength = defineTool({
  name: 'source_length', description: 'test', parameters: Type.Object({ id: Type.String() }), replay: 'safe',
  execute: async (a, api, ctx) => ({ output: [{ type: 'text' as const, text: String((await sourceText(api, a.id, ctx))?.length) }] }),
});
const readSourceExt = defineExtension({ name: 'test', tools: [readSource, sourceLength] });
const delivered = (seen: string[]) => JSON.parse(seen.at(-1)!).content[0].text as string;

test('a report that exhausts its revisions is delivered with a banner and inline marks; a passing one has neither', async () => {
  const base = await web();
  const verdictOf = async (cfg: Parameters<typeof researchSessionExtensions>[0], cites: string[]) => {
    const { session } = await run(cfg, (step) => [
      call('web_fetch', { urls: [`${base}/page`] }),
      ...cites.flatMap((c) => [step(call('write', { path: 'artifacts/report.md', content: report(c) })), step(call('submit_report', {}))]),
      step(fauxAssistantMessage('Done.')),
    ], { isPublic: loopbackOk });
    const last = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['research.report'].versions.at(-1)!;
    await session.close();
    return { content: last.content, v: last.verdict as any };
  };
  const bad = await verdictOf({ maxRevisions: 1 }, ['[s1][s9]', '[s1][s8]']);
  const text = deliveredReport(bad.content, bad.v)!;
  assert.match(text, /^> \*\*Not fully checked\.\*\*/);
  assert.match(text, /gate claims_grounded\.cites_fetched_sources: cites s8/);
  assert.match(text, /- Ducks and geese fly\. \[s1\]\[s8\] \(not checked: s8 never fetched\)/);
  assert.doesNotMatch(text, /s1 never/);

  const good = await verdictOf({ maxRevisions: 1 }, ['[s1]']);
  assert.equal(deliveredReport(good.content, good.v), report('[s1]'));
});

test('a version still being revised is not delivered', () => {
  const v = { passed: false, limitReached: false, checks: [] };
  assert.equal(deliveredReport(report('[s1]'), v), undefined);
  assert.equal(deliveredReport(report('[s1]'), null), undefined);
});

test('a fetched source keeps its text, retrievable by id and keyed by the recorded hash', async () => {
  const base = await web();
  const { seen, session } = await run({ maxFetchBytes: 1000 }, (step) => [
    call('web_fetch', { urls: [`${base}/page`] }),
    step(call('read_source', { id: 's1' })),
    step(call('read_source', { id: 's2' })),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk }, [readSourceExt]);
  assert.match(delivered(seen.slice(0, 2)), /^Ducks\n\nDucks\nDucks & geese fly\.$/);
  assert.equal(delivered(seen.slice(0, 3)), 'none');
  const sources = (await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT))!.sources;
  assert.equal(sources.length, 1);
  await session.close();
});

test('stored source text is capped at 500k characters and still retrievable', async () => {
  const base = await web();
  const { seen, session } = await run({ maxFetchBytes: 1_000_000 }, (step) => [
    call('web_fetch', { urls: [`${base}/huge`] }),
    step(call('source_length', { id: 's1' })),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk }, [readSourceExt]);
  assert.match(delivered(seen.slice(0, 2)), /^500000$/);
  await session.close();
});

test('marks follow what the check saw: a source fetched after the failing check is still marked', async () => {
  const base = await web();
  const { session } = await run({ maxRevisions: 0 }, (step) => [
    call('web_fetch', { urls: [`${base}/page`] }),
    step(call('write', { path: 'artifacts/report.md', content: report('[s2]') })),
    step(call('submit_report', {})),
    step(call('web_fetch', { urls: [`${base}/paper.pdf`] })),
    step(fauxAssistantMessage('Done.')),
  ], { isPublic: loopbackOk });
  const sources = (await session.harness.snapshot(SourcesDoc, session.root.id, BACKGROUND_CONTEXT))!.sources;
  assert.deepEqual(sources.map((x) => x.id), ['s1', 's2'], 's2 exists by render time');
  const last = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['research.report'].versions.at(-1)!;
  const text = deliveredReport(last.content, last.verdict as any)!;
  assert.match(text, /\[s2\] \(not checked: s2 never fetched\)/);
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

const GOOD = '## Summary\nDucks fly.\n## Assumptions\nNone.\n## Findings\n- The page says "Ducks & geese fly." [s1]\n- Ducks are birds. [s1]\n';

const gate = (id: string, ev: Parameters<NonNullable<(typeof researchRubric)['criteria'][0]['checks'][0]['code']>>[1]) => researchRubric.criteria[0].checks.find((c) => c.id === id)!.code!('', ev);
const unit = ({ id = 'f', ...state }: { id?: string } & Record<string, any>) => ({ id, state });

test('findings_cited: every finding bullet needs a [sN]; the bare one is named and marked', () => {
  assert.deepEqual(findingItems(GOOD).map((f) => f.first), ['- The page says "Ducks & geese fly." [s1]', '- Ducks are birds. [s1]']);
  assert.equal(gate('findings_cited', { finding: [unit({ cited: true, first: 'a' })] }).met, 1);
  const bad = gate('findings_cited', { finding: [unit({ id: 'finding-1', cited: true, first: 'a' }), unit({ id: 'finding-2', cited: false, first: '- Ducks swim.' })] });
  assert.equal(bad.met, 0.5);
  assert.deepEqual(bad.marks, ['- Ducks swim.']);
  assert.match(bad.detail!, /finding-2 cite no source/);
  assert.equal(gate('findings_cited', { finding: [] }).met, 0);
});

test('quotes_exist: a quote the cited source lacks fails the gate with the quote as its mark', () => {
  assert.equal(gate('quotes_exist', { finding: [unit({ misquotes: [] })] }).met, 1);
  const bad = gate('quotes_exist', { finding: [unit({ id: 'finding-1', misquotes: [] }), unit({ id: 'finding-2', misquotes: ['Geese never fly'] })] });
  assert.equal(bad.met, 0.5);
  assert.deepEqual(bad.marks, ['Geese never fly']);
  assert.match(bad.detail!, /finding-2 quotes text its cited source does not contain: "Geese never fly"/);
});

test('a report with a real quote passes all gates; an invented quote and a bare finding fail with marks that deliveredReport places', async () => {
  const base = await web();
  const verdictOf = async (content: string) => {
    const { session } = await run({ maxRevisions: 0 }, (step) => [
      call('web_fetch', { urls: [`${base}/page`] }),
      step(call('write', { path: 'artifacts/report.md', content })),
      step(call('submit_report', {})),
      step(fauxAssistantMessage('Done.')),
    ], { isPublic: loopbackOk });
    const last = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['research.report'].versions.at(-1)!;
    await session.close();
    return last;
  };
  const good = await verdictOf(GOOD);
  assert.equal((good.verdict as any).passed, true);
  assert.equal(deliveredReport(good.content, good.verdict as any), GOOD);

  const badContent = GOOD.replace('Ducks are birds. [s1]', 'Ducks swim.').replace('"Ducks & geese fly."', '"Geese never fly south"');
  const bad = await verdictOf(badContent);
  const checks = (bad.verdict as any).checks.filter((c: any) => c.met < 1).map((c: any) => c.id);
  assert.deepEqual(checks, ['findings_cited', 'quotes_exist']);
  const text = deliveredReport(bad.content, bad.verdict as any)!;
  assert.match(text, /^- Ducks swim\. \(no source cited\)$/m);
  assert.match(text, /^- The page says "Geese never fly south" \[s1\] \(quote not found in the cited source\)$/m);
  assert.match(text, /gate claims_grounded\.findings_cited/);
});

import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { createModels, fauxProvider } from '@earendil-works/pi-ai';
import { replayJudge } from '@quack/eval';
import { buildEvidence, decisionModelTokens, rejudge, type Verdict } from '@quack/judge';
import { fakeClef } from '../../contracts/test/clef.ts';
import { inputsOf, manifest, reportDeclaration, researchRubric, type SourceRecord } from '../src/index.ts';
import { linksOf, linksRetrieved, pageKey, type FetchLog } from '../src/links.ts';
import { sentences } from '../src/style.ts';

const SAME: Array<[string, string]> = [
  ['http://example.com/a', 'https://example.com/a'],
  ['https://www.example.com/a', 'https://example.com/a'],
  ['https://example.com/a/', 'https://example.com/a'],
  ['https://example.com/a#section-2', 'https://example.com/a'],
  ['https://example.com/a?utm_source=x&utm_medium=y&utm_campaign=z', 'https://example.com/a'],
  ['https://example.com/a?fbclid=1&gclid=2&ref=hn&sessionid=9', 'https://example.com/a'],
  ['https://example.com/a?b=2&a=1', 'https://example.com/a?a=1&b=2'],
  ['https://EXAMPLE.com/a', 'https://example.com/a'],
  ['https://ex%61mple.com/a', 'https://example.com/a'],
  ['https://example.com/%7Euser/a%20b', 'https://example.com/~user/a b'],
  ['https://example.com/docs/index.html', 'https://example.com/docs/'],
  ['https://example.com/index.htm', 'https://example.com'],
  ['https://m.example.com/a', 'https://example.com/a'],
  ['https://example.com/a/amp', 'https://example.com/a'],
  ['https://example.com/a?amp=1', 'https://example.com/a'],
  ['https://example.com/a;jsessionid=ABC123', 'https://example.com/a'],
  ['https://example.com:443/a', 'https://example.com/a'],
];
const DIFFERENT: Array<[string, string]> = [
  ['https://example.com/a', 'https://example.com/b'],
  ['https://example.com/a', 'https://example.com'],
  ['https://example.com/papers/one.pdf', 'https://example.com/papers/two.pdf'],
  ['https://example.com/item?id=1', 'https://example.com/item?id=2'],
  ['https://example.com/item?page=2', 'https://example.com/item'],
  ['https://example.com/a', 'https://example.org/a'],
  ['https://docs.example.com/a', 'https://example.com/a'],
  ['https://example.com/A', 'https://example.com/a'],
  ['https://example.com:8080/a', 'https://example.com/a'],
];

test('pageKey: one page in its usual disguises gets one key; another path, file, host or meaningful query gets another', () => {
  for (const [a, b] of SAME) assert.equal(pageKey(a), pageKey(b), `${a} ~ ${b}`);
  for (const [a, b] of DIFFERENT) assert.notEqual(pageKey(a), pageKey(b), `${a} !~ ${b}`);
  assert.equal(pageKey('mailto:a@example.com'), undefined);
  assert.equal(pageKey('not a url'), undefined);
});

test('linksOf: inline links, autolinks, bare URLs and reference definitions, each once; code and other schemes are not links', () => {
  const md = [
    '## Summary',
    'See [the spec](https://a.test/spec) and <https://b.test/x>, or https://c.test/y. [s1]',
    'Also [the notes][n] and mail a@b.test.',
    '',
    '[n]: https://d.test/notes',
    '',
    '`https://e.test/in-code` and',
    '```',
    'https://f.test/in-fence',
    '```',
    'Again [the spec](https://a.test/spec).',
  ].join('\n');
  assert.deepEqual(linksOf(md), ['https://a.test/spec', 'https://b.test/x', 'https://c.test/y', 'https://d.test/notes']);
});

const LOG: FetchLog = {
  fetched: [{ id: 's1', url: 'https://docs.example.com/guide/install' }, { id: 's2', url: 'https://example.com/final', asked: 'https://example.com/short' }],
  found: ['https://news.example.org/story?id=7'],
};
const cites = (...lines: string[]) => `## Summary\n${lines.join('\n')}\n`;

test('links_retrieved: a fetched page, the URL a redirect started from, and a search hit pass, in any disguise', () => {
  const md = cites(
    'Install per [the guide](http://www.docs.example.com/guide/install/#linux) [s1].',
    'The short link <https://example.com/short> and its target https://example.com/final?utm_source=x [s2].',
    'Reported in https://news.example.org/story?id=7&fbclid=abc.',
  );
  assert.deepEqual(linksRetrieved(md, LOG), { met: 1 });
});

test('links_retrieved: a page neither fetched nor found fails, naming the URL and the nearest retrieved page; an unknown [sN] fails too', () => {
  const r = linksRetrieved(cites('See https://docs.example.com/guide/upgrade and https://elsewhere.test/x [s1][s9].'), LOG);
  assert.equal(r.met, 0.25);
  assert.equal(r.detail, 'https://docs.example.com/guide/upgrade was neither fetched nor in a search result (the nearest page this session retrieved is https://docs.example.com/guide/install); https://elsewhere.test/x was neither fetched nor in a search result; [s9] is no source id a web_fetch returned. Fetch each page you link with web_fetch, or drop the link.');
  assert.equal(linksRetrieved(cites('Story https://news.example.org/story?id=8 [s1].'), LOG).met, 0.5, 'another id is another story');
  assert.equal(linksRetrieved(cites('The guide root https://docs.example.com/ [s1].'), LOG).met, 0.5, 'a fetched sub-page does not vouch for its parent');
});

test('the inputs carry the fetch log and the search hits, and the gate reads them through the declaration', () => {
  const sources: SourceRecord[] = [{ id: 's1', url: 'https://example.com/b', asked: 'https://example.com/a', fetchedAt: 1, hash: 'h' }];
  const inputs = inputsOf(cites('Per https://example.com/a and https://hits.test/x [s1].'), sources, [{ url: 'https://hits.test/x', at: 2 }]);
  assert.deepEqual(JSON.parse(inputs.log), { fetched: [{ id: 's1', url: 'https://example.com/b', asked: 'https://example.com/a' }], found: ['https://hits.test/x'] });
  const gate = researchRubric.criteria[0].checks[0];
  assert.deepEqual(gate.code!(inputs.report, buildEvidence(reportDeclaration, inputs).evidence), { met: 1 });
});

test('the rubric is the link gate and five lintdec style checks, warnings weighing twice an info', () => {
  assert.deepEqual([researchRubric.name, researchRubric.version, researchRubric.threshold], ['research.report', 15, 0.7]);
  assert.deepEqual(researchRubric.criteria.flatMap((c) => c.checks.map((k) => [`${c.id}.${k.id}`, k.code ? 'code' : 'model', k.weight])), [
    ['links.links_retrieved', 'code', 'gate'],
    ['style.no_purpose', 'model', 2],
    ['style.prose_list', 'model', 1],
    ['style.topic_sentence', 'model', 1],
    ['style.wordy_phrase', 'model', 1],
    ['style.list_not_parallel', 'model', 1],
  ]);
  for (const d of Object.values(reportDeclaration.checks)) assert.deepEqual([d.question.type, d.question.type === 'bool' && d.question.pass, d.decisionModel], ['bool', ['no'], undefined], 'Clef settles each item first');
});

test('sentences: a stop before a capital ends one; an abbreviation or initial does not', () => {
  assert.deepEqual(sentences('Ducks fly. They rest, e.g. on lakes. J. Smith saw 3. Then "Geese" left!'), ['Ducks fly.', 'They rest, e.g. on lakes.', 'J. Smith saw 3.', 'Then "Geese" left!']);
});

const CLEAN = [
  '## Summary',
  'Mallards leave the north in October and fly up to 800 km a day [s1].',
  '',
  '## Assumptions',
  'We read "ducks" as mallards. The survey covers 2025.',
  '',
  '## Findings',
  '- Mallards fly south in October [s1].',
  '- Flocks fly up to 800 km in a day [s1].',
  '- Flocks rest on lakes between legs [s1].',
  '',
].join('\n');
const MESSY = CLEAN
  .replace('The survey covers 2025.', 'The survey covers 2025. We create, update, publish, and delete the counts in four tables, then merge them, then chart them.')
  .replace('- Flocks fly up to 800 km in a day [s1].', '- **Range:** up to 800 km in a day [s1].')
  .replace('- Flocks rest on lakes', '- In order to rest, flocks stop on lakes');
const units = (md: string) => {
  const ev = buildEvidence(reportDeclaration, inputsOf(md, [], [])).evidence;
  return Object.fromEntries(Object.keys(reportDeclaration.checks).map((id) => [id, ev[id].map((u) => `${u.id}: ${Object.values(u.state)[0]}`)]));
};

test('each style check asks one question per unit of its kind: the opening, paragraphs, long paragraphs, marked wordy phrases and mixed lists', () => {
  assert.deepEqual(units(CLEAN), {
    no_purpose: ['no_purpose opening: ## Summary\nMallards leave the north in October and fly up to 800 km a day.'],
    prose_list: ['prose_list paragraph-1: Mallards leave the north in October and fly up to 800 km a day.', 'prose_list paragraph-2: We read "ducks" as mallards. The survey covers 2025.'],
    topic_sentence: [], wordy_phrase: [], list_not_parallel: [],
  });
  const messy = units(MESSY);
  assert.deepEqual(messy.topic_sentence, ['topic_sentence paragraph-2: We read "ducks" as mallards. The survey covers 2025. We create, update, publish, and delete the counts in four tables, then merge them, then chart them.']);
  assert.deepEqual(messy.wordy_phrase, ['wordy_phrase phrase-1: ⟦In order to⟧ rest, flocks stop on lakes between legs.']);
  assert.deepEqual(messy.list_not_parallel, ['list_not_parallel list-1: - Mallards fly south in October.\n- **Range:** up to 800 km in a day.\n- In order to rest, flocks stop on lakes between legs.']);
});

test('wordy_phrase skips a negated phrase, a quotation and code; list_not_parallel skips a list whose items all open alike', () => {
  const md = '## Summary\nIt is not able to fly. Its maker said "we built it in order to fly far" [s1]. Run `in order to` now. It flies prior to dawn.\n\n## Findings\n- **A:** one [s1].\n- **B:** two [s1].\n';
  assert.deepEqual(units(md).wordy_phrase, ['wordy_phrase phrase-1: It flies ⟦prior to⟧ dawn.']);
  assert.deepEqual(units(md).list_not_parallel, []);
});

const FLAWS = ['create, update', '⟦In order to⟧', '**Range:**'];
// Clef sees a planted flaw in a unit's text, so it answers yes, the problem is there, and no otherwise.
const flawed = (_q: string, state: unknown) => (FLAWS.some((f) => JSON.stringify(state).includes(f)) ? 0.95 : 0.05);

async function judged(t: Parameters<typeof fakeClef>[0], md: string): Promise<Verdict> {
  const models = createModels();
  models.setProvider(fauxProvider({ provider: 'judge', models: [{ id: 'j' }] }).provider);
  const clef = await fakeClef(t, undefined, flawed);
  models.setProvider(clef.provider);
  let verdict: Verdict | undefined;
  await replayJudge({
    cases: [{ case: 'ducks', kind: 'research.report', content: md, inputs: inputsOf(md, [{ id: 's1', url: 'https://birds.test/mallards', fetchedAt: 1, hash: 'h' }], []) }],
    arms: [{ label: 'clef', adapters: manifest.evalAdapters!, rejudge: async (...a) => (verdict = await rejudge(...a)), decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }],
    repeat: 1, concurrency: 1, models, judgeModel: { provider: 'judge', modelId: 'j' },
  });
  return verdict!;
}

test('a clean report passes on Clef alone; one wordy phrase still passes; a report full of style problems fails', async (t) => {
  const clean = await judged(t, CLEAN);
  assert.deepEqual([clean.passed, clean.score], [true, 1]);
  const one = await judged(t, CLEAN.replace('- Flocks rest on lakes', '- In order to rest, flocks stop on lakes'));
  assert.deepEqual([one.passed, one.score], [true, 0.75]);
  const messy = await judged(t, MESSY);
  assert.equal(messy.passed, false);
  assert.deepEqual(messy.checks.map((k) => [k.id, k.met, k.resolvers]), [
    ['links_retrieved', 1, { code: 1 }],
    ['no_purpose', 1, { clef: 1 }],
    ['prose_list', 0.5, { clef: 2 }],
    ['topic_sentence', 0, { clef: 1 }],
    ['wordy_phrase', 0, { clef: 1 }],
    ['list_not_parallel', 0, { clef: 1 }],
  ]);
  assert.match(messy.checks[4].notes[0], /^wordy_phrase phrase-1: flagged by clef \(no reason given\): "⟦In order to⟧ rest/);
});

test('every style unit of the fixture reports fits Clef\'s window', () => {
  for (const name of readdirSync(new URL('./fixtures/reports/', import.meta.url))) {
    const md = readFileSync(new URL(`./fixtures/reports/${name}`, import.meta.url), 'utf8');
    const ev = buildEvidence(reportDeclaration, inputsOf(md, [], [])).evidence;
    for (const [id, d] of Object.entries(reportDeclaration.checks)) {
      const over = ev[id].filter((u) => decisionModelTokens(u, [{ id: `style.${id}`, question: d.question }]) >= 8192).map((u) => u.id);
      assert.deepEqual(over, [], `${name} ${id}`);
    }
  }
});

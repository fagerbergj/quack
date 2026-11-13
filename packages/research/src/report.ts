import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { registerSelector, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, type Inputs, type Question, type Rubric, type SelectorDecl, type SplitDecl } from '@quack/judge';
import type { ArtifactKind, RecordedSession, SealedArtifact } from '@quack/sdk';
import { sourcePath, SourcesDoc, type SourceRecord } from './tools.ts';

const CITATION = /\[(s\d+)\]/g;

/** The source ids a text cites, each once, in order of first citation. */
export const citedIds = (text: string) => [...new Set([...text.matchAll(CITATION)].map((m) => m[1]))];

const SECTIONS = ['summary', 'assumptions', 'findings'];

/** Structure problems, all at once; whether the citations are real is the judge's gate, since it needs the session's fetch log. */
export function validateReport(text: string): string[] {
  const headings = new Set([...text.matchAll(/^## (\w+)/gm)].map((m) => m[1].toLowerCase()));
  const problems = SECTIONS.filter((s) => !headings.has(s)).map((s) => `missing a "## ${s[0].toUpperCase()}${s.slice(1)}" section`);
  if (!citedIds(text).length) problems.push('no source is cited; cite fetched sources as [s1], [s2]');
  return problems;
}

const unmark = (s: string) => s.replace(/\s*\[s\d+\]/g, '').replace(/[*_`#>]/g, '').replace(/\s+/g, ' ').trim();

/** The first line of "## Summary", else the first heading; source marks and markdown are stripped. */
export function summarizeReport(text: string): string {
  const body = /^## Summary\s*$([\s\S]*?)(?=^## |(?![\s\S]))/im.exec(text)?.[1] ?? '';
  const line = body.split('\n').map(unmark).find(Boolean);
  return line ?? unmark(/^#+ .+$/m.exec(text)?.[0] ?? '');
}

export const reportKind: ArtifactKind = { kind: 'research.report', path: 'artifacts/report.md', tool: 'submit_report', validate: validateReport, summarize: summarizeReport };

const ITEM = /^(\s*)([-*]|\d+\.)\s/;
type Item = { indent: number; lines: string[]; context: string[]; kids: number };
type Parse = { items: Item[]; open: Item[]; para: string[]; intro: string[]; gap: boolean; fence: boolean };
// A bullet ending in a colon (marks and emphasis aside) that has sub-bullets only introduces them: it is their context, not a finding.
const leads = (i: Item) => i.kids > 0 && /:$/.test(i.lines.join(' ').replace(/\s*\[s\d+\]/g, '').replace(/[\s*_`]+$/, ''));

function bullet(p: Parse, line: string, indent: number) {
  p.open = p.open.filter((o) => o.indent < indent);
  const parent = p.open.at(-1);
  // A list's lead-in is the paragraph directly above it; a code block or table between them does not part them.
  if (!parent && p.para.length) [p.intro, p.para] = [p.para, []];
  if (parent) parent.kids++;
  // Context is there to cite the bullet; an unmarked lead line would only reframe the claim for the judge.
  const marked = (lines: string[]) => (/\[s\d+\]/.test(lines.join(' ')) ? lines : []);
  const item = { indent, lines: [line], context: parent ? [...parent.context, ...marked(parent.lines)] : marked(p.intro), kids: 0 };
  p.items.push(item);
  p.open.push(item);
}

function readLine(p: Parse, line: string) {
  const m = ITEM.exec(line);
  if (/^```/.test(line)) [p.fence, p.open] = [!p.fence, []];
  else if (p.fence || /^\|/.test(line)) p.open = [];
  else if (m) bullet(p, line, m[1].length);
  else if (/^#/.test(line)) [p.open, p.para, p.intro] = [[], [], []];
  else if (line.trim() && p.open.length && (!p.gap || /^\s{2}/.test(line))) p.open.at(-1)!.lines.push(line);
  else if (line.trim()) [p.open, p.para] = [[], p.gap ? [line] : [...p.para, line]];
  p.gap = !line.trim();
}

/** Every bullet under a "## Findings…" heading, with its first line (which names it in feedback) and as text its own lines after its context:
 *  the bullets it sits in and the paragraph above its list, whose marks cite it too. */
export function findingItems(report: string): Array<{ first: string; text: string }> {
  const p: Parse = { items: [], open: [], para: [], intro: [], gap: false, fence: false };
  for (const m of report.matchAll(/^## Findings\b.*$([\s\S]*?)(?=^## |(?![\s\S]))/gim)) for (const line of `${m[1]}\n#`.split('\n')) readLine(p, line);
  return p.items.filter((i) => !leads(i)).map((i) => ({ first: i.lines[0].trim(), text: [...i.context, ...i.lines].join('\n') }));
}

/** Only the words, in order: punctuation, quote style, dashes, markdown emphasis and link targets differ between a page and a faithful quote of it. */
// Space-padded so a quote matches whole words: "must" must not pass inside "mustn't", nor "able" inside "unable".
const words = (s: string) => ` ${(s.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1').toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []).join(' ')} `;
const MIN_QUOTE = 12;

type Quote = { start: number; end: number; inner: string };

/** The quotes of a text, paired within a line. A straight double quote right after a digit with no quote open is an inch mark (19" rails), not
 *  an opening; with one open it closes it ("…in 2024"). */
function quotesOf(text: string): Quote[] {
  const out: Quote[] = [];
  let open = -1;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    const closes = open >= 0 && (ch === '"' || ch === '”');
    if (ch === '\n') open = -1;
    else if (closes && i > open + 1) out.push({ start: open, end: i + 1, inner: text.slice(open + 1, i) });
    if (closes) open = -1;
    else if (open < 0 && (ch === '“' || (ch === '"' && !/\d/.test(text[i - 1] ?? '')))) open = i;
  }
  return out;
}

// A line break, or a sentence end before a capital or a figure; a period inside a quote is masked out first, so it ends nothing.
const BOUNDARY = /\n|[.!?][”’)]?\s+(?=["“(*_]?[\p{Lu}\p{N}])/gu;
const CITES = /\[s\d+\]/;

/** Whether the sentence a quote sits in cites a source; `masked` is the text with every quote's inside blanked, so only real sentence ends count. */
function citedSentence(text: string, masked: string, q: Quote): boolean {
  let from = 0;
  let to = text.length;
  for (const m of masked.matchAll(BOUNDARY)) {
    if (m.index + m[0].length <= q.start) from = m.index + m[0].length;
    else if (m.index >= q.end) {
      to = m.index + 1;
      break;
    }
  }
  return CITES.test(text.slice(from, to));
}

/** Quoted spans, split at elisions and [bracketed] insertions, edge punctuation dropped; short ones are scare quotes or names. Only a quote whose
 *  sentence cites a source is checked: a quote is attributed by marking its sentence, so one in an uncited sentence is the writer's wording. */
export function quotedSpans(text: string): string[] {
  const quotes = quotesOf(text);
  const masked = quotes.reduce((t, q) => `${t.slice(0, q.start + 1)}${'x'.repeat(q.inner.length)}${t.slice(q.end - 1)}`, text);
  // A bullet with no mark of its own is cited by its lead-in, so every quote in it is that source's.
  const own = text.slice(Math.max(0, text.search(/(^|\n)\s*([-*]|\d+\.)\s(?![\s\S]*\n\s*([-*]|\d+\.)\s)/)));
  const attributed = (q: Quote) => !CITES.test(own) || citedSentence(text, masked, q);
  return quotes
    .filter((q) => q.inner.length >= MIN_QUOTE && attributed(q))
    .flatMap((q) => q.inner.split(/\.\.\.|…|\[[^\]]*\]/).map((p) => p.replace(/^[\s,.;:!?]+|[\s,.;:!?]+$/g, '')).filter((p) => p.length >= 6));
}

/** The finding's quoted spans that none of the cited source texts contain, word for word. */
export function missingQuotes(finding: string, texts: string[]): string[] {
  const pages = texts.map(words);
  return quotedSpans(finding).filter((q) => !pages.some((t) => t.includes(words(q))));
}

/** A finding named by its opening words; its bullet id means nothing to a reader or the reviser. */
function opening(first: string | undefined): string {
  const words = (first ?? '').replace(/^\s*([-*]|\d+\.)\s+/, '').replace(/\s*\[s\d+\]/g, '').split(/\s+/).filter(Boolean);
  return `The finding starting "${words.slice(0, 8).join(' ')}${words.length > 8 ? '…' : ''}"`;
}

const STOP_CHARS = 4000;
const PIECE = 600;
// A pair of the claim's words in its order is a phrase: it weighs more than the two words apart.
const PAIR = 2;
// Words a kept piece already showed count a quarter, so the next pick covers what the claim says that nothing shown says yet.
const SEEN = 0.25;
// Value is per character spent, plus a floor so a bare label does not outrank the sentence that says the thing.
const COST = 100;
const POOL = 200;

const terms = (s: string) => s.toLowerCase().match(/\d[\d.,]*\d|\d|\p{L}[\p{L}\p{N}'’]{2,}/gu) ?? [];
const pairs = (ts: string[]) => ts.slice(1).map((t, i) => `${ts[i]} ${t}`);

/** Each piece's claim words and phrases, weighted by how rare they are on the page, so a name or figure outweighs a word every paragraph uses. */
function weigh(pieces: string[], claim: string) {
  const ct = terms(claim);
  const want = new Set([...ct, ...pairs(ct)]);
  const hits = pieces.map((p) => { const ts = terms(p); return [...new Set([...ts, ...pairs(ts)])].filter((w) => want.has(w)); });
  const df = new Map<string, number>();
  for (const h of hits) for (const w of h) df.set(w, (df.get(w) ?? 0) + 1);
  const weight = (w: string) => Math.log(1 + pieces.length / df.get(w)!) * (w.includes(' ') ? PAIR : 1);
  return { hits, weight };
}

/** The pieces kept, in page order: contiguous ones joined by a newline, a gap marked with an ellipsis line. */
const joinKept = (pieces: string[], kept: Set<number>) => [...kept].sort((a, b) => a - b).map((i, k, xs) => (k && xs[k - 1] !== i - 1 ? `…\n${pieces[i]}` : pieces[i])).join('\n');

/** The passages of a source that say what the claim says, at most `max` characters of a 500k page: the piece richest in the claim's rare words and phrases,
 *  with its neighbours, then the piece adding most that nothing kept shows yet, until the budget is spent. */
export function excerpt(text: string, claim: string, max = STOP_CHARS): string {
  if (text.length <= max) return text;
  const pieces = text.split(/\n+/).flatMap((l) => l.match(new RegExp(`[\\s\\S]{1,${PIECE}}`, 'g')) ?? []);
  const { hits, weight } = weigh(pieces, claim);
  const seen = new Set<string>();
  const kept = new Set<number>();
  const value = (i: number) => hits[i].reduce((n, w) => n + weight(w) * (seen.has(w) ? SEEN : 1), 0) / (pieces[i].length + COST);
  const fits = (i: number) => i >= 0 && i < pieces.length && !kept.has(i) && joinKept(pieces, new Set([...kept, i])).length <= max;
  // The budget holds a few dozen pieces, so only the best first scores compete; rescoring all of a 500k page per pick is quadratic.
  const pool = pieces.map((_, i) => i).filter((i) => hits[i].length).sort((a, b) => value(b) - value(a) || a - b).slice(0, POOL);
  while (pool.length) {
    const [best] = pool.splice(pool.reduce((b, i, k) => (value(i) > value(pool[b]) ? k : b), 0), 1);
    // With its neighbours: the heading or sentence beside a passage often names what it is about.
    if (fits(best)) for (const i of [best, best - 1, best + 1]) if (fits(i)) (kept.add(i), hits[i].forEach((w) => seen.add(w)));
  }
  // No shared wording: the page's opening is still better evidence than nothing.
  return kept.size ? joinKept(pieces, kept) : text.slice(0, max);
}

type Entry = { kind: string; model?: Array<{ content?: unknown }> };
const textOf = (c: unknown) => (typeof c === 'string' ? c : Array.isArray(c) ? c.map((b) => (b as { text?: string }).text ?? '').join('') : '');

/** The person's first message in this session: the research request the report must answer. */
export async function requestText(api: ToolExecutionApi, ctx: Context): Promise<string | undefined> {
  const page = await api.commit((tx) => tx.scanEntries({ conversationId: api.conversationId, order: 'ascending' }, 50, undefined), ctx);
  const first = (page.items as unknown as Entry[]).find((e) => e.kind === 'pi.user');
  return textOf(first?.model?.[0]?.content).trim() || undefined;
}

/** The judge's named texts: the report and the request whole, the kept text of each cited source by id, and the ids this session fetched. */
export function inputsOf(report: string, request: string | undefined, texts: Map<string, string>, fetchedIds: string[]): Inputs {
  return { report, request: request ?? '', sources: JSON.stringify(Object.fromEntries(texts)), fetched: JSON.stringify(fetchedIds) };
}

/** The inputs from the fetch log, a reader of the kept source files and the request, wherever those come from. */
async function inputsFrom(report: string, sources: SourceRecord[], read: (path: string) => Promise<string | undefined>, request: string | undefined): Promise<Inputs> {
  const texts = new Map<string, string>();
  for (const id of citedIds(report)) {
    const rec = sources.find((s) => s.id === id);
    const t = rec && (await read(sourcePath(rec.hash)));
    if (t !== undefined) texts.set(id, t);
  }
  return inputsOf(report, request, texts, sources.map((s) => s.id));
}

export function reportInputs() {
  return async (api: ToolExecutionApi, sealed: { content: string }, ctx: Context): Promise<Inputs> => {
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources ?? [];
    const read = async (path: string) => {
      const r = await api.env?.readTextFile(path, ctx);
      return r?.ok ? r.value : undefined;
    };
    return inputsFrom(sealed.content, fetched, read, await requestText(api, ctx));
  };
}

/** The same inputs from a recording, as they stood when the report was sealed: sources fetched later were not in the log yet. */
export async function recordedReportInputs(session: RecordedSession, content: string): Promise<Inputs> {
  const sealed = session.artifacts[reportKind.kind]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the report is not a version this session sealed, so what it had fetched by then is unknown');
  if (!session.readFile || !session.inputs) throw new Error('the recording keeps no workspace or no messages, so the fetched sources or the request are missing');
  const sources = ((session.docs['research.sources']?.sources ?? []) as unknown as SourceRecord[]).filter((s) => s.fetchedAt <= sealed.sealedAt);
  return inputsFrom(content, sources, session.readFile, session.inputs[0]?.trim() || undefined);
}

// Each finding is an item named by its place in the report, so a replay case can name one.
const findingChunks = (inputs: Inputs, o: SplitDecl): Chunk[] => findingItems(inputs[String(o.from)]).map((f, i) => ({ id: `finding-${i + 1}`, text: f.text }));
registerSplitter('research.findings', findingChunks);

/** The passages of each cited source whose text was kept that say what the finding says; undefined, dropping the finding, when none was kept. */
function sourceExcerpts(inputs: Inputs, chunk: Chunk, o: SelectorDecl): string | undefined {
  const texts = JSON.parse(inputs[(o as { from: string }).from]) as Record<string, string>;
  const ids = citedIds(chunk.text).filter((id) => Object.hasOwn(texts, id));
  if (!ids.length) return undefined;
  return JSON.stringify(ids.map((id) => ({ id, text: excerpt(texts[id], chunk.text) })));
}
registerSelector('research.excerpts', sourceExcerpts);

const sourceTexts = (ev: Evidence) => (ev.sources?.[0]?.state.texts ?? {}) as Record<string, string>;

/** Per finding: its first line, whether it cites a source, and the quotes no kept cited source contains (one whose text was not kept, a failed write or a fork, cannot disprove a quote). */
function findingFacts(report: string, ev: Evidence) {
  const texts = sourceTexts(ev);
  return findingItems(report).map((f) => {
    const ids = citedIds(f.text);
    const kept = ids.filter((id) => Object.hasOwn(texts, id));
    return { first: f.first, cited: ids.length > 0, misquotes: kept.length ? missingQuotes(f.text, kept.map((id) => texts[id])) : [] };
  });
}

function findingsCited(artifact: string, ev: Evidence): CodeResult {
  const all = findingFacts(artifact, ev);
  const bare = all.filter((f) => !f.cited);
  if (!all.length) return { met: 0, detail: 'the Findings section has no bullets; list each finding as "- claim [s1]"' };
  return { met: (all.length - bare.length) / all.length, detail: bare.length ? `${bare.map((f) => `${opening(f.first)} cites no source`).join('; ')}. Add the [sN] of the page that supports each, or drop it.` : undefined };
}

function quotesExist(artifact: string, ev: Evidence): CodeResult {
  const all = findingFacts(artifact, ev);
  const wrong = all.filter((f) => f.misquotes.length);
  const listed = wrong.map((f) => `${opening(f.first)} quotes ${f.misquotes.slice(0, 3).map((q) => `"${q}"`).join(', ')}, which isn't on the cited page`).join('; ');
  return { met: all.length ? (all.length - wrong.length) / all.length : 1, detail: wrong.length ? `${listed}. Quote the page as written, or paraphrase without quotation marks.` : undefined };
}

function citesFetchedSources(artifact: string, ev: Evidence): CodeResult {
  const fetched = new Set((ev.fetched?.[0]?.state.ids ?? []) as string[]);
  const cited = citedIds(artifact);
  const unfetched = cited.filter((id) => !fetched.has(id));
  return { met: cited.length ? (cited.length - unfetched.length) / cited.length : 0, detail: unfetched.length ? `cites ${unfetched.join(', ')}, which this session never fetched; fetch the page with web_fetch or drop the claim` : cited.length ? undefined : 'cites no source' };
}

const SUPPORTED_WEIGHT = 4;
const CONTRADICTED_WEIGHT = -4;
// Audit weights are 4 and 3; 0 (observe-only) until their false-block rate on good reports is under 10%.
const ASKS_WEIGHT = 0;
const SUMMARY_WEIGHT = 0;
const SELF_WEIGHT = -4;

// The judge's knowledge has a cutoff, so a real fact newer than it reads as invented; every question says so.
const FAIR = ' Judge only from the text shown; a fact you do not recognise is not evidence of error.';
const CITE = '\\s*\\[s\\d+\\]';
const bool = (instructions: string): Question => ({ type: 'bool', instructions: `${instructions}${FAIR}` });
// A pitfall is asked as the defect itself, its item holding on "no" (the fold's `pass`). Its options say what each answer means:
// a generic "Yes, this holds." beside a question about a defect can be read as the item holding or as the defect being there.
const defect = (instructions: string, yes: string, no: string): Question => ({ type: 'choice', instructions: `${instructions}${FAIR}`, options: { yes, no } });

/** How each model check of researchRubric is asked: per finding with its sources' excerpts, per Summary sentence with every bullet outside
 *  the Summary and Assumptions, and of the request and of the report once each, with the whole report. */
export const reportDeclaration: Declaration = {
  scopes: {
    fetched: { split: { name: 'whole', from: 'fetched' }, piece: 'ids' },
    sources: { split: { name: 'whole', from: 'sources' }, piece: 'texts' },
    // Nothing is shared, so each finding is its own request: shown the request, or batched with others, the 27B passed more unsupported findings.
    claim: { split: { name: 'research.findings', from: 'report' }, piece: 'finding', context: { sources: { name: 'research.excerpts', from: 'sources' } } },
    request: { split: { name: 'whole', from: 'request' }, piece: 'request', context: { report: { name: 'whole', from: 'report' } } },
    // Every sentence sees every finding, once per request, rather than the few that share its words.
    summary: {
      split: { name: 'sentences', from: 'report', sections: ['Summary'], strip: CITE },
      piece: 'sentence',
      context: { findings: { name: 'items', from: 'report', skip: ['Summary', 'Assumptions'], strip: CITE } },
    },
    report: { split: { name: 'whole', from: 'report' }, piece: 'report' },
  },
  checks: {
    // Clef passes findings their source does not state or states differently (a ranking, a figure, an author), so the 27B decides both.
    supported_by_source: { scope: 'claim', aggregate: { name: 'share' }, decisionModel: false, question: bool('Does the fetched text of the source this finding cites state what the finding claims?') },
    contradicted_by_source: { scope: 'claim', aggregate: { name: 'share', pass: ['no'] }, decisionModel: false, question: defect('Does the fetched text of the source this finding cites contradict it?', 'Yes: the source says something different from the finding.', 'No: the source does not say otherwise.') },
    answers_every_ask: { scope: 'request', aggregate: { name: 'all' }, question: bool("Is every distinct ask and constraint in the request (place, timeframe, budget, audience) answered in the report's Summary and Findings, or named under Assumptions as unsettled?") },
    summary_follows_findings: { scope: 'summary', aggregate: { name: 'all' }, question: bool("Does this sentence of the report's Summary follow from the findings shown with it?") },
    no_self_contradiction: { scope: 'report', aggregate: { name: 'all', pass: ['no'] }, question: defect('Does any figure, date or name in the report conflict with another passage of the report?', 'Yes: two passages give different figures, dates or names for the same thing.', 'No: they all agree.') },
  },
};

// Weight 0 is observe-only: the check is reported but does not score.
export const researchRubric: Rubric = {
  name: 'research.report',
  version: 6,
  threshold: 0.8,
  criteria: [
    { id: 'claims_grounded', checks: [
      { id: 'cites_fetched_sources', label: "Cites a source that wasn't opened", weight: 'gate', code: citesFetchedSources },
      { id: 'findings_cited', label: "A finding cites no source", weight: 'gate', code: findingsCited },
      { id: 'quotes_exist', label: "Quotes text that isn't in the source", weight: 'gate', code: quotesExist },
      { id: 'supported_by_source', label: "A finding isn't stated by its source", weight: SUPPORTED_WEIGHT },
      { id: 'contradicted_by_source', label: "A finding is contradicted by its source", weight: CONTRADICTED_WEIGHT },
    ] },
    { id: 'answers_question', checks: [
      { id: 'answers_every_ask', label: "Leaves part of the question unanswered", weight: ASKS_WEIGHT },
    ] },
    { id: 'internally_consistent', checks: [
      { id: 'summary_follows_findings', label: "Summary says more than the findings", weight: SUMMARY_WEIGHT },
      { id: 'no_self_contradiction', label: "Report contradicts itself", weight: SELF_WEIGHT },
    ] },
  ],
};

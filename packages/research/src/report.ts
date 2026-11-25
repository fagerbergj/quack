import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { registerSelector, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, type Inputs, type Question, type Rubric, type SelectorDecl, type SplitDecl } from '@quack/judge';
import type { ArtifactKind, RecordedSession, SealedArtifact } from '@quack/sdk';
import { citedIds, fenced, findingItems, sectionBody, sentencesOf, splitReport, type Section } from '@quack/sdk/report';
import { sourcePath, SourcesDoc, type SourceRecord } from './tools.ts';

export { citedIds, findingItems, sectionBody, splitReport };

const SECTIONS = ['summary', 'assumptions', 'findings'];
const heading = (s: string) => `## ${s[0].toUpperCase()}${s.slice(1)}`;

/** Why a required section is not there exactly once, naming the headings that look like it but do not count, so the writer can fix them. */
function sectionProblem(text: string, sections: Section[], s: string): string[] {
  const n = sections.filter((x) => x.name === s).length;
  if (n > 1) return [`"${heading(s)}" appears ${n} times; a report has one "${heading(s)}" section, so merge them`];
  if (n) return [];
  const near = fenced(text).filter(([line, code]) => !code && /^#/.test(line) && line.replace(/^#+/, '').trim().toLowerCase().startsWith(s)).map(([line]) => `"${line.trim()}"`);
  if (!near.length) return [`missing a "${heading(s)}" section`];
  return [`missing a "${heading(s)}" section: the heading must read exactly "${heading(s)}", once, and ${near.join(', ')} ${near.length > 1 ? 'do' : 'does'} not count`];
}

/** Structure problems, all at once; whether the citations are real is the judge's gate, since it needs the session's fetch log. */
export function validateReport(text: string): string[] {
  const { sections } = splitReport(text);
  const problems = SECTIONS.flatMap((s) => sectionProblem(text, sections, s));
  if (!citedIds(text).length) problems.push('no source is cited; cite fetched sources as [s1], [s2]');
  return problems;
}

const unmark = (s: string) => s.replace(/\s*\[s\d+\]/g, '').replace(/[*_`#>]/g, '').replace(/\s+/g, ' ').trim();

/** The first line of "## Summary", else the first heading; source marks and markdown are stripped. */
export function summarizeReport(text: string): string {
  const line = sectionBody(text, ['summary']).split('\n').map(unmark).find(Boolean);
  return line ?? unmark(/^#+ .+$/m.exec(text)?.[0] ?? '');
}

export const reportKind: ArtifactKind = { kind: 'research.report', path: 'artifacts/report.md', tool: 'submit_report', validate: validateReport, summarize: summarizeReport };

/** Only the words, in order: punctuation, quote style, dashes, markdown emphasis and link targets differ between a page and a faithful quote of it. */
// Space-padded so a quote matches whole words; a kept JSON source's \n escapes are dropped, so an escape's n does not fuse with the quoted word.
const words = (s: string) => ` ${(s.replace(/\\./g, ' ').replace(/\[([^\]]*)\]\([^)]*\)/g, '$1').toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []).join(' ')} `;
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

/** The judge's named texts: the report whole and the kept text of each cited source by id. */
export function inputsOf(report: string, texts: Map<string, string>): Inputs {
  return { report, sources: JSON.stringify(Object.fromEntries(texts)) };
}

/** The inputs from the fetch log, a reader of the kept source files and the request, wherever those come from. */
async function inputsFrom(report: string, sources: SourceRecord[], read: (path: string) => Promise<string | undefined>): Promise<Inputs> {
  const texts = new Map<string, string>();
  for (const id of citedIds(report)) {
    const rec = sources.find((s) => s.id === id);
    const t = rec && (await read(sourcePath(rec.hash)));
    if (t !== undefined) texts.set(id, t);
  }
  return inputsOf(report, texts);
}

export function reportInputs() {
  return async (api: ToolExecutionApi, sealed: { content: string }, ctx: Context): Promise<Inputs> => {
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources ?? [];
    const read = async (path: string) => {
      const r = await api.env?.readTextFile(path, ctx);
      return r?.ok ? r.value : undefined;
    };
    return inputsFrom(sealed.content, fetched, read);
  };
}

/** The same inputs from a recording, as they stood when the report was sealed: sources fetched later were not in the log yet. */
export async function recordedReportInputs(session: RecordedSession, content: string): Promise<Inputs> {
  const sealed = session.artifacts[reportKind.kind]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the report is not a version this session sealed, so what it had fetched by then is unknown');
  if (!session.readFile) throw new Error('the recording keeps no workspace, so the fetched sources are missing');
  const sources = ((session.docs['research.sources']?.sources ?? []) as unknown as SourceRecord[]).filter((s) => s.fetchedAt <= sealed.sealedAt);
  return inputsFrom(content, sources, session.readFile);
}

let parsed: [string, Record<string, string>] | undefined;
// Every item of a report reads the same sources input, so it is parsed once, not per item.
const textsIn = (raw: string) => (parsed?.[0] === raw ? parsed[1] : (parsed = [raw, JSON.parse(raw) as Record<string, string>])[1]);

/** One item per sentence of a finding and kept source it cites (its own marks, else the finding's), grouped by the sentence ("finding-3.2");
 *  a sentence of several shares its whole finding, so its items pack and Clef reads what "it" refers to. */
function claimSources(inputs: Inputs, o: SplitDecl): Chunk[] {
  const [reportIn, sourcesIn] = [o.from].flat();
  const kept = new Set(Object.keys(textsIn(inputs[sourcesIn])));
  return findingItems(inputs[reportIn]).flatMap((f, i) => {
    const sentences = sentencesOf(f.text);
    return sentences.flatMap((t, k) => {
      const claim = `finding-${i + 1}.${k + 1}`;
      const ids = (citedIds(t).length ? citedIds(t) : citedIds(f.text)).filter((id) => kept.has(id));
      return ids.map((id) => ({ id: `${claim} ~ ${id}`, key: id, text: t.replace(/\s*\[s\d+\]/g, '').trim(), meta: { claim, ...(sentences.length > 1 && { finding: f.text }) } }));
    });
  });
}
registerSplitter('research.claimSources', claimSources);

/** The report's parsed form, as the research report artifact contract states it: its sections by name, the ids it cites and its findings. */
export function parseReport(report: string): { sections: Record<string, string>; citedIds: string[]; findings: Array<{ first: string; text: string }> } {
  const keys = [...new Set(splitReport(report).sections.map((s) => s.name))];
  return { sections: Object.fromEntries(keys.map((k) => [k, sectionBody(report, [k])])), citedIds: citedIds(report), findings: findingItems(report) };
}

/** The passage of the item's source that says most nearly what its claim says. */
registerSelector('research.sourceExcerpt', (inputs, chunk, o) => JSON.stringify({ id: chunk.key, text: excerpt(textsIn(inputs[(o as { from: string }).from])[chunk.key!], chunk.text) }));

const sourceTexts = (ev: Evidence) => (ev.sources?.[0]?.state.texts ?? {}) as Record<string, string>;

/** Per finding: its first line, whether it cites a source, and the quotes no kept cited source contains (one whose text was not kept, a failed write or a fork, cannot disprove a quote). */
function findingFacts(report: string, ev: Evidence) {
  const texts = sourceTexts(ev);
  return findingItems(report).map((f) => {
    const ids = citedIds(f.text);
    const kept = ids.filter((id) => Object.hasOwn(texts, id));
    return { first: f.first, cited: ids.length > 0, misquotes: kept.length ? missingQuotes(f.text, kept.map((id) => texts[id])) : [], pages: kept.map((id) => texts[id]) };
  });
}

function findingsCited(artifact: string, ev: Evidence): CodeResult {
  const all = findingFacts(artifact, ev);
  const bare = all.filter((f) => !f.cited);
  if (!all.length) return { met: 0, detail: 'the Findings section has no bullets; list each finding as "- claim [s1]"' };
  return { met: (all.length - bare.length) / all.length, detail: bare.length ? `${bare.map((f) => `${opening(f.first)} cites no source`).join('; ')}. Add the [sN] of the page that supports each, or drop it.` : undefined };
}

// The 8-word page window sharing the most words with the quote: where the page says what the quote should have quoted.
const NEAR = 8;
const nearestOn = (pages: string[], quote: string): string | undefined => {
  const want = new Set(quote.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []);
  let best: string | undefined;
  let hits = 0;
  for (const page of pages) {
    // A kept JSON source keeps its escapes; drop them so an escape's characters are not page words.
    // Distinct words: a window repeating one stopword must not outscore the window holding the quote's content word.
    const ws = page.replace(/\\u[0-9a-fA-F]{4}|\\./g, ' ').match(/[\p{L}\p{N}]+/gu) ?? [];
    for (let i = 0; i + NEAR <= ws.length; i++) {
      const same = new Set(ws.slice(i, i + NEAR).map((w) => w.toLowerCase()).filter((w) => want.has(w))).size;
      if (same > hits) [best, hits] = [ws.slice(i, i + NEAR).join(' '), same];
    }
  }
  return best;
};

function quotesExist(artifact: string, ev: Evidence): CodeResult {
  const all = findingFacts(artifact, ev);
  const wrong = all.filter((f) => f.misquotes.length);
  const listed = wrong.map((f) => {
    const quotes = f.misquotes.slice(0, 3).map((q) => {
      const near = nearestOn(f.pages, q);
      return near ? `"${q}" (nearest on the cited page: "${near}")` : `"${q}"`;
    }).join(', ');
    return `${opening(f.first)} quotes ${quotes}, which isn't on the cited page`;
  }).join('; ');
  return { met: all.length ? (all.length - wrong.length) / all.length : 1, detail: wrong.length ? `${listed}. Quote the page as written, or paraphrase without quotation marks.` : undefined };
}

// The score is the share of claims some cited source states; at 0.8, a fifth of a report's claims may go unstated.
const SUPPORTED_WEIGHT = 1;

// The judge's knowledge has a cutoff, so a real fact newer than it reads as invented; every question says so.
const FAIR = ' Judge only from the text shown; a fact you do not recognise is not evidence of error.';
// One question per claim and source: Clef passed unstated findings when one item held a whole finding and every source it cites.
// The report's own reasoning is on no page, so an inference that follows from the finding's facts passes; the facts are each judged on their own.
const STATES: Question = {
  type: 'choice',
  instructions: `Does this source state the claim? The finding, when shown, gives what the claim refers to and the facts the report states beside it; judge this claim, not the others.${FAIR}`,
  options: {
    states: 'The source states what the claim says.',
    contradicts: 'The source says something different from the claim.',
    reasoning: "The claim is the report's own inference, not a fact about the world, and it follows from facts the finding states. A claim that gives a number, date, name or quote the finding's other sentences do not is a fact, never reasoning, however it is worded.",
    silent: 'The source does not say this.',
  },
};

/** How each model check of researchRubric is asked: per sentence of a finding and kept source it cites, with that source's excerpt. */
export const reportDeclaration: Declaration = {
  scopes: {
    sources: { split: { name: 'whole', from: 'sources' }, piece: 'texts' },
    source: { split: { name: 'research.claimSources', from: ['report', 'sources'] }, piece: 'claim', context: { source: { name: 'research.sourceExcerpt', from: 'sources' } } },
  },
  checks: {
    // A claim holds once one source states it or it is reasoning that follows; one that a source contradicts and none states fails as any unstated claim does.
    supported_by_source: { scope: 'source', aggregate: { name: 'grouped', groupBy: 'claim', inner: 'any', outer: 'share', pass: ['states', 'reasoning'] }, question: STATES },
  },
};

export const researchRubric: Rubric = {
  name: 'research.report',
  version: 14,
  threshold: 0.8,
  criteria: [
    { id: 'claims_grounded', checks: [
      { id: 'findings_cited', label: "A finding cites no source", weight: 'gate', code: findingsCited },
      { id: 'quotes_exist', label: "Quotes text that isn't in the source", weight: 'gate', code: quotesExist },
      { id: 'supported_by_source', label: "A claim its sources don't back", weight: SUPPORTED_WEIGHT },
    ] },
  ],
};

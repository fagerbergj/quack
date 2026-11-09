import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { ArtifactKind, RecordedSession, SealedArtifact } from '@quack/sdk';
import { citedIds } from './marks.ts';
import { sourcePath, SourcesDoc, type SourceRecord } from './tools.ts';

export { citedIds };

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

export const reportKind: ArtifactKind = { kind: 'research.report', path: 'artifacts/report.md', validate: validateReport, summarize: summarizeReport };

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
  const item = { indent, lines: [line], context: parent ? [...parent.context, ...parent.lines] : p.intro, kids: 0 };
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

/** Every bullet under a "## Findings…" heading, with its first line (what a mark points at) and as text its own lines after its context:
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

/** Quoted spans of a finding, split at elisions and [bracketed] insertions; shorter ones are scare quotes or names, not evidence. Edge punctuation is dropped: American style puts the sentence's comma or period inside the marks. */
export function quotedSpans(text: string): string[] {
  return [...text.matchAll(/["“]([^"”\n]+)["”]/g)]
    .filter((m) => m[1].length >= MIN_QUOTE)
    .flatMap((m) => m[1].split(/\.\.\.|…|\[[^\]]*\]/).map((p) => p.replace(/^[\s,.;:!?]+|[\s,.;:!?]+$/g, '')).filter((p) => p.length >= 6));
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

/** All the judge's evidence from what the session kept: per finding its cited flag and misquotes (code gates) and, when a cited source's text was kept, its windows (claim checks, which would fail a finding with no text for nothing); the report and request whole. */
export function evidenceOf(report: string, request: string | undefined, texts: Map<string, string>, fetchedIds: string[]): Evidence {
  const finding: Evidence[string] = [];
  const claim: Evidence[string] = [];
  for (const [i, f] of findingItems(report).entries()) {
    const ids = citedIds(f.text);
    const kept = ids.filter((id) => texts.has(id));
    // A source whose text was not kept (a failed write, a fork) cannot disprove a quote, so it is skipped.
    const misquotes = kept.length ? missingQuotes(f.text, kept.map((id) => texts.get(id)!)) : [];
    finding.push({ id: `finding-${i + 1}`, state: { finding: f.text, cited: ids.length > 0, first: f.first, misquotes } });
    if (kept.length) claim.push({ id: `finding-${i + 1}`, state: { finding: f.text, sources: kept.map((id) => ({ id, text: excerpt(texts.get(id)!, f.text) })) } });
  }
  return {
    review: [{ id: 'report', state: { report } }],
    sources: [{ id: 'fetched', state: { ids: fetchedIds } }],
    finding,
    claim,
    request: request ? [{ id: 'request', whole: true, state: { request, report } }] : [],
  };
}

/** The evidence from the fetch log, a reader of the kept source files and the request, wherever those come from. */
async function evidenceFrom(report: string, sources: SourceRecord[], read: (path: string) => Promise<string | undefined>, request: string | undefined): Promise<Evidence> {
  const texts = new Map<string, string>();
  for (const id of citedIds(report)) {
    const rec = sources.find((s) => s.id === id);
    const t = rec && (await read(sourcePath(rec.hash)));
    if (t !== undefined) texts.set(id, t);
  }
  return evidenceOf(report, request, texts, sources.map((s) => s.id));
}

export function reportEvidence() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Evidence> => {
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources ?? [];
    const read = async (path: string) => {
      const r = await api.env?.readTextFile(path, ctx);
      return r?.ok ? r.value : undefined;
    };
    return evidenceFrom(sealed.content, fetched, read, await requestText(api, ctx));
  };
}

/** The same evidence from a recording, as it stood when the report was sealed: sources fetched later were not in the log yet. */
export async function recordedReportEvidence(session: RecordedSession, content: string): Promise<Evidence> {
  const sealed = session.artifacts[reportKind.kind]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the report is not a version this session sealed, so what it had fetched by then is unknown');
  if (!session.readFile || !session.inputs) throw new Error('the recording keeps no workspace or no messages, so the fetched sources or the request are missing');
  const sources = ((session.docs['research.sources']?.sources ?? []) as unknown as SourceRecord[]).filter((s) => s.fetchedAt <= sealed.sealedAt);
  return evidenceFrom(content, sources, session.readFile, session.inputs[0]?.trim() || undefined);
}

type FindingState = { cited: boolean; first: string; misquotes: string[] };
const findings = (ev: Evidence) => (ev.finding ?? []).map((u) => ({ id: u.id, ...(u.state as unknown as FindingState) }));

function findingsCited(_artifact: string, ev: Evidence): CodeResult {
  const all = findings(ev);
  const bare = all.filter((f) => !f.cited);
  if (!all.length) return { met: 0, detail: 'the Findings section has no bullets; list each finding as "- claim [s1]"' };
  return { met: (all.length - bare.length) / all.length, marks: bare.map((f) => f.first), detail: bare.length ? `${bare.map((f) => `${opening(f.first)} cites no source`).join('; ')}. Add the [sN] of the page that supports each, or drop it.` : undefined };
}

function quotesExist(_artifact: string, ev: Evidence): CodeResult {
  const all = findings(ev);
  const wrong = all.filter((f) => f.misquotes.length);
  const listed = wrong.map((f) => `${opening(f.first)} quotes ${f.misquotes.slice(0, 3).map((q) => `"${q}"`).join(', ')}, which isn't on the cited page`).join('; ');
  return { met: all.length ? (all.length - wrong.length) / all.length : 1, marks: wrong.flatMap((f) => f.misquotes), detail: wrong.length ? `${listed}. Quote the page as written, or paraphrase without quotation marks.` : undefined };
}

function citesFetchedSources(artifact: string, ev: Evidence): CodeResult {
  const fetched = new Set((ev.sources?.[0]?.state.ids ?? []) as string[]);
  const cited = citedIds(artifact);
  const unfetched = cited.filter((id) => !fetched.has(id));
  return { met: cited.length ? (cited.length - unfetched.length) / cited.length : 0, marks: unfetched, detail: unfetched.length ? `cites ${unfetched.join(', ')}, which this session never fetched; fetch the page with web_fetch or drop the claim` : cited.length ? undefined : 'cites no source' };
}

const SUPPORTED_WEIGHT = 4;
const CONTRADICTED_WEIGHT = -4;
// Audit weights are 4 and 3; 0 (observe-only) until their false-block rate on good reports is under 10% (Rubric v2 measurement).
const ASKS_WEIGHT = 0;
const SUMMARY_WEIGHT = 0;
const SELF_WEIGHT = -4;

// The judge's own knowledge goes stale (a real 2025 fact flagged as invented, 86833b31), so every question says so.
const FAIR = ' Judge only from the text shown; a fact you do not recognise is not evidence of error.';

// Weight 0 is observe-only: the check is reported but does not score.
export const researchRubric: Rubric = {
  name: 'research.report',
  version: 5,
  threshold: 0.8,
  criteria: [
    { id: 'claims_grounded', checks: [
      { id: 'cites_fetched_sources', label: "Cites a source that wasn't opened", weight: 'gate', code: citesFetchedSources },
      { id: 'findings_cited', label: "A finding cites no source", weight: 'gate', code: findingsCited },
      { id: 'quotes_exist', label: "Quotes text that isn't in the source", weight: 'gate', code: quotesExist },
      { id: 'supported_by_source', label: "A finding isn't stated by its source", weight: SUPPORTED_WEIGHT, scope: 'claim', question: `Does the fetched text of the source this finding cites state what the finding claims?${FAIR}` },
      { id: 'contradicted_by_source', label: "A finding is contradicted by its source", weight: CONTRADICTED_WEIGHT, scope: 'claim', question: `Does the fetched text of the source this finding cites contradict it?${FAIR}` },
    ] },
    { id: 'answers_question', checks: [
      { id: 'answers_every_ask', label: "Leaves part of the question unanswered", weight: ASKS_WEIGHT, scope: 'request', question: `Is every distinct ask and constraint in the request (place, timeframe, budget, audience) answered in the report's Summary and Findings, or named under Assumptions as unsettled?${FAIR}` },
    ] },
    { id: 'internally_consistent', checks: [
      { id: 'summary_follows_findings', label: "Summary says more than the findings", weight: SUMMARY_WEIGHT, scope: 'review', question: `Does every claim in the Summary follow from a Finding?${FAIR}` },
      { id: 'no_self_contradiction', label: "Report contradicts itself", weight: SELF_WEIGHT, scope: 'review', question: `Does any figure, date or name in the report conflict with another passage of the report?${FAIR}` },
    ] },
  ],
};

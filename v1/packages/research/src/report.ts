import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { ArtifactKind, SealedArtifact } from '@quack/sdk';
import { citedIds } from './marks.ts';
import { sourceText, SourcesDoc } from './tools.ts';

export { citedIds };

const SECTIONS = ['summary', 'assumptions', 'findings'];

/** Structure problems, all at once (ART-2); whether the citations are real is the judge's gate, since it needs the session's fetch log. */
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

/** The bullets under "## Findings", each with its first line (what a mark points at) and any continuation lines. */
export function findingItems(report: string): Array<{ first: string; text: string }> {
  const body = /^## Findings\s*$([\s\S]*?)(?=^## |(?![\s\S]))/im.exec(report)?.[1] ?? '';
  const items: string[][] = [];
  for (const line of body.split('\n')) {
    if (/^\s*([-*]|\d+\.)\s/.test(line)) items.push([line]);
    else if (items.length && line.trim()) items.at(-1)!.push(line);
  }
  return items.map((l) => ({ first: l[0].trim(), text: l.join('\n') }));
}

const squash = (s: string) => s.replace(/\s+/g, ' ').trim().toLowerCase().replace(/[‘’]/g, "'").replace(/[“”]/g, '"').replace(/`/g, '');
const MIN_QUOTE = 12;

/** Quoted spans of a finding, split at elisions; shorter ones are scare quotes or names, not evidence. Edge punctuation is dropped: American style puts the sentence's comma or period inside the marks. */
export function quotedSpans(text: string): string[] {
  return [...text.matchAll(/["“]([^"”\n]+)["”]/g)]
    .filter((m) => m[1].length >= MIN_QUOTE)
    .flatMap((m) => m[1].split(/\.\.\.|…/).map((p) => p.replace(/^[\s,.;:!?]+|[\s,.;:!?]+$/g, '')).filter((p) => p.length >= 6));
}

/** The finding's quoted spans that none of the cited source texts contain. */
export const missingQuotes = (finding: string, texts: string[]) => quotedSpans(finding).filter((q) => !texts.map(squash).some((t) => t.includes(squash(q))));

/** A finding named by its opening words; its bullet id means nothing to a reader or the reviser. */
function opening(first: string | undefined): string {
  const words = (first ?? '').replace(/^\s*([-*]|\d+\.)\s+/, '').replace(/\s*\[s\d+\]/g, '').split(/\s+/).filter(Boolean);
  return `The finding starting "${words.slice(0, 8).join(' ')}${words.length > 8 ? '…' : ''}"`;
}

const STOP_CHARS = 4000;
const PIECE = 600;

/** The parts of a source most like the claim (shared numbers and words), in page order, so the judge reads a window rather than 500k characters. */
export function excerpt(text: string, claim: string, max = STOP_CHARS): string {
  if (text.length <= max) return text;
  const want = new Set(claim.toLowerCase().match(/\d[\d.,]*|[a-z]{4,}/g) ?? []);
  const pieces = text.split(/\n+/).flatMap((l) => l.match(new RegExp(`[\\s\\S]{1,${PIECE}}`, 'g')) ?? []);
  const scored = pieces.map((t, i) => ({ t, i, n: new Set((t.toLowerCase().match(/\d[\d.,]*|[a-z]{4,}/g) ?? []).filter((w) => want.has(w))).size })).sort((a, b) => b.n - a.n || a.i - b.i);
  const keep: typeof scored = [];
  let size = 0;
  for (const p of scored) if (p.n && size + p.t.length <= max) (keep.push(p), (size += p.t.length));
  // No shared wording: the page's opening is still better evidence than nothing.
  const kept = keep.length ? keep.sort((a, b) => a.i - b.i).map((p) => p.t).join('\n…\n') : text.slice(0, max);
  return kept;
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

export function reportEvidence() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Evidence> => {
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources ?? [];
    const texts = new Map<string, string>();
    for (const id of citedIds(sealed.content)) {
      const t = await sourceText(api, id, ctx);
      if (t !== undefined) texts.set(id, t);
    }
    return evidenceOf(sealed.content, await requestText(api, ctx), texts, fetched.map((s) => s.id));
  };
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

// Weights are the audit's (docs/v1/audits/research.md); the Rubric v2 measurement there says which are scored or observe-only (weight 0).
export const researchRubric: Rubric = {
  name: 'research.report',
  version: 3,
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

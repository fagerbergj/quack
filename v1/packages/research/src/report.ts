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

export const reportKind: ArtifactKind = { kind: 'research.report', path: 'artifacts/report.md', validate: validateReport };

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

const squash = (s: string) => s.replace(/\s+/g, ' ').trim().toLowerCase().replace(/[‘’]/g, "'").replace(/[“”]/g, '"');
const MIN_QUOTE = 12;

/** Quoted spans of a finding, split at elisions; shorter ones are scare quotes or names, not evidence. */
export function quotedSpans(text: string): string[] {
  return [...text.matchAll(/["“]([^"”\n]+)["”]/g)]
    .filter((m) => m[1].length >= MIN_QUOTE)
    .flatMap((m) => m[1].split(/\.\.\.|…/).map((p) => p.trim()).filter((p) => p.length >= 6));
}

export function reportEvidence() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Evidence> => {
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources ?? [];
    const finding: Evidence[string] = [];
    for (const [i, f] of findingItems(sealed.content).entries()) {
      const ids = citedIds(f.text);
      // A source whose text was not kept (a failed write, a fork) cannot disprove a quote, so it is skipped.
      const texts = (await Promise.all(ids.map((id) => sourceText(api, id, ctx)))).filter((t): t is string => t !== undefined).map(squash);
      const misquotes = texts.length ? quotedSpans(f.text).filter((q) => !texts.some((t) => t.includes(squash(q)))) : [];
      finding.push({ id: `finding-${i + 1}`, cited: ids.length > 0, first: f.first, misquotes, state: { finding: f.text } } as never);
    }
    return { review: [{ id: 'report', state: { report: sealed.content } }], sources: [{ id: 'fetched', state: { ids: fetched.map((s) => s.id) } }], finding };
  };
}

type FindingUnit = { id: string; cited: boolean; first: string; misquotes: string[] };
const findings = (ev: Evidence) => (ev.finding ?? []) as unknown as FindingUnit[];

function findingsCited(_artifact: string, ev: Evidence): CodeResult {
  const all = findings(ev);
  const bare = all.filter((f) => !f.cited);
  if (!all.length) return { met: 0, detail: 'the Findings section has no bullets; list each finding as "- claim [s1]"' };
  return { met: (all.length - bare.length) / all.length, marks: bare.map((f) => f.first), detail: bare.length ? `${bare.map((f) => f.id).join(', ')} cite no source; add the [sN] of the page that supports it or drop the finding` : undefined };
}

function quotesExist(_artifact: string, ev: Evidence): CodeResult {
  const all = findings(ev);
  const wrong = all.filter((f) => f.misquotes.length);
  const listed = wrong.map((f) => `${f.id} quotes text its cited source does not contain: ${f.misquotes.slice(0, 3).map((q) => `"${q}"`).join(', ')}`).join('; ');
  return { met: all.length ? (all.length - wrong.length) / all.length : 1, marks: wrong.flatMap((f) => f.misquotes), detail: wrong.length ? `${listed}. Quote the page as written, or paraphrase without quotation marks.` : undefined };
}

function citesFetchedSources(artifact: string, ev: Evidence): CodeResult {
  const fetched = new Set((ev.sources?.[0]?.state.ids ?? []) as string[]);
  const cited = citedIds(artifact);
  const unfetched = cited.filter((id) => !fetched.has(id));
  return { met: cited.length ? (cited.length - unfetched.length) / cited.length : 0, marks: unfetched, detail: unfetched.length ? `cites ${unfetched.join(', ')}, which this session never fetched; fetch the page with web_fetch or drop the claim` : cited.length ? undefined : 'cites no source' };
}

// Code gates only; the model-judged claim-support checks wait for a labelled set (docs/v1/audits/research.md).
export const researchRubric: Rubric = {
  name: 'research.report',
  version: 2,
  threshold: 0.8,
  criteria: [{ id: 'claims_grounded', checks: [
    { id: 'cites_fetched_sources', weight: 'gate', code: citesFetchedSources },
    { id: 'findings_cited', weight: 'gate', code: findingsCited },
    { id: 'quotes_exist', weight: 'gate', code: quotesExist },
  ] }],
};

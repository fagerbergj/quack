import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { ArtifactKind, SealedArtifact } from '@quack/sdk';
import { SourcesDoc } from './tools.ts';

const SECTIONS = ['summary', 'assumptions', 'findings'];
const CITATION = /\[(s\d+)\]/g;

export const citedIds = (text: string) => [...new Set([...text.matchAll(CITATION)].map((m) => m[1]))];

/** Structure problems, all at once (ART-2); whether the citations are real is the judge's gate, since it needs the session's fetch log. */
export function validateReport(text: string): string[] {
  const headings = new Set([...text.matchAll(/^## (\w+)/gm)].map((m) => m[1].toLowerCase()));
  const problems = SECTIONS.filter((s) => !headings.has(s)).map((s) => `missing a "## ${s[0].toUpperCase()}${s.slice(1)}" section`);
  if (!citedIds(text).length) problems.push('no source is cited; cite fetched sources as [s1], [s2]');
  return problems;
}

export const reportKind: ArtifactKind = { kind: 'research.report', path: 'artifacts/report.md', validate: validateReport };

export function reportEvidence() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Evidence> => {
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources ?? [];
    return { review: [{ id: 'report', state: { report: sealed.content } }], sources: [{ id: 'fetched', state: { ids: fetched.map((s) => s.id) } }] };
  };
}

function citesFetchedSources(artifact: string, ev: Evidence): CodeResult {
  const fetched = new Set((ev.sources?.[0]?.state.ids ?? []) as string[]);
  const cited = citedIds(artifact);
  const unfetched = cited.filter((id) => !fetched.has(id));
  return { met: cited.length ? (cited.length - unfetched.length) / cited.length : 0, detail: unfetched.length ? `cites ${unfetched.join(', ')}, which this session never fetched; fetch the page with web_fetch or drop the claim` : cited.length ? undefined : 'cites no source' };
}

// Claim-support checks against the fetched text come with the sources store that keeps it; today the gate is provenance only.
export const researchRubric: Rubric = {
  name: 'research.report',
  version: 1,
  threshold: 0.8,
  criteria: [{ id: 'claims_grounded', checks: [{ id: 'cites_fetched_sources', weight: 'gate', code: citesFetchedSources }] }],
};

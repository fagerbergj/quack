import type { Context } from '@earendil-works/chord/context';
import { Type } from '@earendil-works/pi-ai';
import { defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { feedback, type CodeResult, type Evidence, type Rubric, type Verdict } from '@quack/judge';
import { sealedVersion, type ArtifactKind, type SealedArtifact } from '@quack/sdk';
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

/** The report as delivered: a version that failed at the limit gets a banner and a mark on each line citing a source never fetched (RES-4). */
export function markUnchecked(content: string, v: Verdict, fetched: string[]): string {
  const known = new Set(fetched);
  const lines = content.split('\n').map((l) => {
    const bad = citedIds(l).filter((id) => !known.has(id));
    return bad.length ? `${l} (not checked: ${bad.join(', ')} never fetched)` : l;
  });
  const failed = feedback(v.checks).map((l) => `> - ${l}`).join('\n');
  return `> **Not fully checked.** The revision limit was reached with these checks failing:\n${failed}\n\n${lines.join('\n')}`;
}

export const deliverReportTool = defineTool({
  name: 'deliver_report',
  description: 'Return a checked version of the report as it is delivered. Only a version submit_report passed, or released at the revision limit, can be delivered.',
  parameters: Type.Object({ version: Type.Integer({ minimum: 1 }) }),
  replay: 'safe',
  async execute(args, api, ctx) {
    const sealed = await sealedVersion(api, reportKind.kind, args.version, ctx);
    const v = sealed?.verdict as unknown as Verdict | null | undefined;
    if (!sealed || !v || !(v.passed || v.limitReached)) return { output: [{ type: 'text' as const, text: `report v${args.version} has not passed its checks; revise and call submit_report` }], isError: true };
    const fetched = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources.map((s) => s.id) ?? [];
    return { output: [{ type: 'text' as const, text: v.passed ? sealed.content : markUnchecked(sealed.content, v, fetched) }] };
  },
});

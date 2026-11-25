import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { Declaration, Evidence, Inputs, Rubric } from '@quack/judge';
import type { ArtifactKind, RecordedSession } from '@quack/sdk';
import { citedIds, fenced, findingItems, sectionBody, splitReport, type Section } from '@quack/sdk/report';
import { linksRetrieved, type FetchLog } from './links.ts';
import { styleChecks, styleDecls, styleScopes } from './style.ts';
import { SourcesDoc, type Found, type SourceRecord } from './tools.ts';

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

/** Structure problems, all at once; whether the citations and links are real is the judge's gate, since it needs the session's fetch log. */
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

/** The judge's named texts: the report whole, and the fetch log and search hits it may link to. */
export function inputsOf(report: string, sources: SourceRecord[], found: Found[]): Inputs {
  const log: FetchLog = { fetched: sources.map((s) => ({ id: s.id, url: s.url, ...(s.asked && { asked: s.asked }) })), found: found.map((f) => f.url) };
  return { report, log: JSON.stringify(log) };
}

export function reportInputs() {
  return async (api: ToolExecutionApi, sealed: { content: string }, ctx: Context): Promise<Inputs> => {
    const doc = await api.snapshot(SourcesDoc, api.conversationId, ctx);
    return inputsOf(sealed.content, doc?.sources ?? [], doc?.found ?? []);
  };
}

/** The same inputs from a recording, as they stood when the report was sealed: pages fetched or found later were not in the log yet. */
export async function recordedReportInputs(session: RecordedSession, content: string): Promise<Inputs> {
  const sealed = session.artifacts[reportKind.kind]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the report is not a version this session sealed, so what it had fetched by then is unknown');
  const doc = (session.docs['research.sources'] ?? {}) as unknown as { sources?: SourceRecord[]; found?: Found[] };
  return inputsOf(content, (doc.sources ?? []).filter((s) => s.fetchedAt <= sealed.sealedAt), (doc.found ?? []).filter((f) => f.at <= sealed.sealedAt));
}

/** The report's parsed form, as the research report artifact contract states it: its sections by name, the ids it cites and its findings. */
export function parseReport(report: string): { sections: Record<string, string>; citedIds: string[]; findings: Array<{ first: string; text: string }> } {
  const keys = [...new Set(splitReport(report).sections.map((s) => s.name))];
  return { sections: Object.fromEntries(keys.map((k) => [k, sectionBody(report, [k])])), citedIds: citedIds(report), findings: findingItems(report) };
}

const logOf = (ev: Evidence) => ev.log[0].state.log as unknown as FetchLog;

/** How each model check of researchRubric is asked; the log scope only carries the fetch log to the link gate. */
export const reportDeclaration: Declaration = {
  scopes: { log: { split: { name: 'whole', from: 'log' }, piece: 'log' }, ...styleScopes },
  checks: styleDecls,
};

// A clean report scores 1; a share of a check's units with its problem costs that share of the check's weight.
export const researchRubric: Rubric = {
  name: 'research.report',
  version: 15,
  threshold: 0.7,
  criteria: [
    { id: 'links', checks: [{ id: 'links_retrieved', label: 'Links a page it never fetched or found', weight: 'gate', code: (report, ev) => linksRetrieved(report, logOf(ev)) }] },
    { id: 'style', checks: styleChecks },
  ],
};

import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { Inputs } from '@quack/judge';
import type { ArtifactKind, SealedArtifact } from '@quack/sdk';
import { PullRequestDoc } from './checkout.ts';

export const LABELS = ['blocking', 'suggestion', 'nit', 'question'] as const;
export const VERDICTS = ['approve', 'comment', 'request_changes'] as const;

export type Finding = { index: number; label: string; anchor?: string; path?: string; line?: number; text: string };
export type ParsedReview = { sections: Record<string, string>; verdict?: string; findings: Finding[]; unparsed: string[] };

export const FINDING = /^- (\w+)(?:\s*\((\w+)\))?:\s*(.*)$/;
// A path with a directory or an extension, a root dotfile (.gitignore), or a capitalized root file (Dockerfile), so `node:18` is not one.
const CITATION = /^`?((?:\.{1,2}\/)?[\w.@+-]*\/[\w.@/+-]+|[\w@+-][\w@+-]*\.[\w.@+-]+|\.[\w@+-][\w.@+-]*|[A-Z][\w-]*):(\d+)(?:-\d+)?`?\s*[—–-]?\s*(.*)$/s;

export const FENCE = /^(```|~~~)/;
// A '- ' line that is no finding, or a '* ' / '+ ' / '1. ' item: a list the gates would never see.
export const BULLET = /^(- |[*+] |\d+\. )/;

/** The findings under `## Findings`; a fenced block is the finding's continuation, whatever its lines start with. */
function parseFindings(section: string): Pick<ParsedReview, 'findings' | 'unparsed'> {
  const findings: Finding[] = [];
  const unparsed: string[] = [];
  let fenced = false;
  for (const line of section.split('\n')) {
    const fence = FENCE.test(line);
    const inside = fenced || fence;
    if (fence) fenced = !fenced;
    const m = inside ? null : FINDING.exec(line);
    if (m) {
      const cite = CITATION.exec(m[3]);
      findings.push({ index: findings.length + 1, label: m[1], anchor: m[2], path: cite?.[1], line: cite ? Number(cite[2]) : undefined, text: cite?.[3] ?? m[3] });
    } else if (!inside && BULLET.test(line)) unparsed.push(line);
    else if (findings.length && (inside || line.trim())) findings.at(-1)!.text += `\n${line}`;
  }
  // The blank lines that end an unclosed fence are the section's padding, not part of the block.
  if (fenced && findings.length) findings.at(-1)!.text = findings.at(-1)!.text.replace(/\n+$/, '');
  return { findings, unparsed };
}

/** Splits a review into its `## ` sections and parses its findings; tolerant, so the validator can name what is wrong. */
export function parseReview(text: string): ParsedReview {
  const sections: Record<string, string> = {};
  let current = '';
  for (const line of text.split('\n')) {
    const heading = /^## (\w+)/.exec(line);
    if (heading) current = heading[1].toLowerCase();
    else if (current) sections[current] = `${sections[current] ?? ''}${line}\n`;
  }
  const { findings, unparsed } = parseFindings(sections.findings ?? '');
  return { sections, verdict: sections.verdict?.trim().split(/\s/)[0]?.toLowerCase(), findings, unparsed };
}

/** Structure problems, all at once; content problems are the judge's to find. */
export function validateReview(text: string): string[] {
  const r = parseReview(text);
  const problems = ['takeaway', 'verdict', 'findings'].filter((s) => !(s in r.sections)).map((s) => `missing a "## ${s[0].toUpperCase()}${s.slice(1)}" section`);
  if ('verdict' in r.sections && !VERDICTS.includes(r.verdict as never)) problems.push(`the verdict must be one of ${VERDICTS.join(', ')}`);
  for (const f of r.findings) if (!LABELS.includes(f.label as never)) problems.push(`finding ${f.index}: label must be one of ${LABELS.join(', ')}`);
  // A finding line the parser cannot read would be invisible to every gate, so it is a structure problem, not a skip.
  for (const line of r.unparsed) problems.push(`not a finding line ("- label (anchor): path:line — text"): ${line.slice(0, 80)}`);
  return problems;
}

const VERDICT_NAMES: Record<string, string> = { approve: 'Approve', comment: 'Comment', request_changes: 'Request changes' };
const PLURAL: Record<string, string> = { blocking: 'blocking', suggestion: 'suggestions', nit: 'nits', question: 'questions' };

/** "1 suggestion, 2 nits": finding counts by severity, empty when there are none. */
export function countFindings(findings: Finding[]): string {
  return LABELS.flatMap((l) => {
    const n = findings.filter((f) => f.label === l).length;
    return n ? [`${n} ${n === 1 ? l : PLURAL[l]}`] : [];
  }).join(', ');
}

/** "Approve · 1 nit": the verdict, then finding counts by severity. */
export function summarizeReview(text: string): string {
  const r = parseReview(text);
  return [VERDICT_NAMES[r.verdict ?? ''] ?? 'Review', countFindings(r.findings) || 'no findings'].join(' · ');
}

export const reviewKind: ArtifactKind = { kind: 'github.review', path: 'artifacts/review.md', tool: 'submit_review', validate: validateReview, outcome: (text) => parseReview(text).verdict, summarize: summarizeReview, basis: async (api, ctx) => (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr?.headSha };

const EXCERPT_LINES = 20;

/** Maps a cited path to a tracked one: as given, minus the checkout dir name, or a unique tail at a `/` boundary. */
export function resolveCitedPath(files: string[], repoName: string, cited: string): string | undefined {
  const bare = cited.startsWith(`${repoName}/`) ? cited.slice(repoName.length + 1) : cited;
  if (files.includes(bare)) return bare;
  const tails = files.filter((f) => f.endsWith(`/${bare}`));
  return tails.length === 1 ? tails[0] : undefined;
}

type Tracked = { files?: string[] };

// A git read the judge's evidence rests on: a failure throws, so a review is never scored against missing evidence.
async function gitRead(api: ToolExecutionApi, repoDir: string, args: string[], ctx: Context): Promise<string> {
  let out = '';
  const r = await api.env!.exec(['git', '-C', repoDir, ...args], { onOutput: (t) => { out += t; } }, ctx);
  if (!r.ok || r.value.exitCode !== 0) throw new Error(`git ${args[0]} in ${repoDir} failed: ${r.ok ? out.slice(-300) : r.error.message}`);
  return out;
}

export async function trackedFiles(api: ToolExecutionApi, repoDir: string, ctx: Context): Promise<string[]> {
  return (await gitRead(api, repoDir, ['ls-files'], ctx)).split('\n').filter(Boolean);
}

async function excerpt(api: ToolExecutionApi, repoDir: string, cited: Pick<Finding, 'path' | 'line'>, ctx: Context, tracked: Tracked = {}) {
  const none = { exists: false, excerpt: '', file: '', path: cited.path };
  if (!cited.path || !cited.line) return none;
  let path = cited.path;
  let read = await api.env!.readTextFile(`${repoDir}/${path}`, ctx);
  if (!read.ok) {
    tracked.files ??= await trackedFiles(api, repoDir, ctx);
    const found = resolveCitedPath(tracked.files, repoDir.split('/').pop()!, cited.path);
    if (!found) return none;
    path = found;
    read = await api.env!.readTextFile(`${repoDir}/${path}`, ctx);
    if (!read.ok) return none;
  }
  const lines = read.value.split('\n');
  if (cited.line < 1 || cited.line > lines.length) return { ...none, file: read.value, path };
  const from = Math.max(1, cited.line - EXCERPT_LINES);
  const text = lines.slice(from - 1, cited.line + EXCERPT_LINES).map((l, i) => `${from + i}: ${l}`).join('\n');
  return { exists: true, excerpt: text, file: read.value, path };
}

// A path:line named in a finding's text: a path with a directory, a file with an extension, or a root dotfile. A bare root file
// (Dockerfile:12) is left out so that prose like TODO:12 does not read as a path.
const MENTION = /(?<![\w./@+-])((?:\.{1,2}\/)?[\w.@+-]*\/[\w.@/+-]*\w|[\w@+-]+\.[A-Za-z][\w.@+-]*\w|\.[\w@+-][\w.@+-]*\w):(\d+)/g;
const NO_LINES = '(the cited lines do not exist)';
export type Citation = { group: string; finding: string; cite: string; code: string };

// One excerpt per path:line the finding names, its own citation first; a mention that does not resolve is left out, since
// cites_existing_lines already holds the finding to its own citation.
async function citationsOf(api: ToolExecutionApi, repoDir: string, f: Finding, own: Awaited<ReturnType<typeof excerpt>>, text: string, ctx: Context, tracked: Tracked): Promise<Citation[]> {
  const out = [{ group: String(f.index), finding: text, cite: `${own.path ?? '?'}:${f.line ?? '?'}`, code: own.excerpt || NO_LINES }];
  const seen = new Set([`${f.path}:${f.line}`, out[0].cite]);
  for (const [, path, line] of f.text.matchAll(MENTION)) {
    if (seen.has(`${path}:${line}`)) continue;
    const e = await excerpt(api, repoDir, { path, line: Number(line) }, ctx, tracked);
    if (!e.exists || seen.has(`${e.path}:${line}`)) continue;
    seen.add(`${e.path}:${line}`);
    out.push({ ...out[0], cite: `${e.path}:${line}`, code: e.excerpt });
  }
  return out;
}

export const diffOf = (api: ToolExecutionApi, repoDir: string, baseRef: string, ctx: Context) => gitRead(api, repoDir, ['diff', `origin/${baseRef}...HEAD`], ctx);

export type ReviewFacts = { ci: string; findings: Array<{ id: string; cited: boolean }> };

// The named texts the declaration reads: the review, the change it reviewed, each finding with its cited code, and the facts code found.
export function reviewInputs(repoDir = 'repo') {
  return async (api: ToolExecutionApi, sealed: { content: string }, ctx: Context): Promise<Inputs> => {
    const r = parseReview(sealed.content);
    const pr = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr;
    const diff = pr ? await diffOf(api, repoDir, pr.baseRef, ctx) : '';
    const tracked: Tracked = {};
    const facts: ReviewFacts = { ci: pr ? pr.checks.map((c) => `${c.name}=${c.conclusion ?? c.status}`).join(', ') : '(unknown)', findings: [] };
    const findings = [];
    const citations: Citation[] = [];
    for (const f of r.findings) {
      const e = await excerpt(api, repoDir, f, ctx, tracked);
      const id = String(f.index);
      facts.findings.push({ id, cited: e.exists });
      const text = `${f.label}${f.anchor ? ` (${f.anchor})` : ''}: ${e.path ?? '?'}:${f.line ?? '?'} ${f.text}`;
      findings.push({ id, blocking: f.label === 'blocking', text, citedCode: e.excerpt || NO_LINES });
      citations.push(...(await citationsOf(api, repoDir, f, e, text, ctx, tracked)));
    }
    return {
      review: sealed.content,
      description: pr ? `${pr.title}\n\n${pr.body}` : '(unknown)',
      issues: pr ? pr.linkedIssues.map((i) => `## Linked issue #${i.number}: ${i.title}\n${i.body}`).join('\n\n') : '',
      diff,
      findings: JSON.stringify(findings),
      citations: JSON.stringify(citations),
      facts: JSON.stringify(facts),
    };
  };
}

import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { Inputs } from '@quack/judge';
import type { ArtifactKind, SealedArtifact } from '@quack/sdk';
import { PullRequestDoc } from './checkout.ts';

export const LABELS = ['blocking', 'suggestion', 'nit', 'question'] as const;
export const ANCHORS = ['defect', 'delivery', 'security', 'design', 'scope', 'tests'] as const;
export const VERDICTS = ['approve', 'comment', 'request_changes'] as const;

export type Finding = { index: number; label: string; anchor?: string; path?: string; line?: number; text: string };
export type ParsedReview = { sections: Record<string, string>; verdict?: string; findings: Finding[]; unparsed: string[] };

export const FINDING = /^- (\w+)(?:\s*\((\w+)\))?:\s*(.*)$/;
// A path with a directory or an extension, or a capitalized root file (Dockerfile, Makefile), so `node:18` is not one.
const CITATION = /^`?((?:\.{1,2}\/)?[\w.@+-]*\/[\w.@/+-]+|[\w@+-][\w@+-]*\.[\w.@+-]+|[A-Z][\w-]*):(\d+)(?:-\d+)?`?\s*[—–-]?\s*(.*)$/s;

/** Splits a review into its `## ` sections and parses its findings; tolerant, so the validator can name what is wrong. */
export function parseReview(text: string): ParsedReview {
  const sections: Record<string, string> = {};
  let current = '';
  for (const line of text.split('\n')) {
    const heading = /^## (\w+)/.exec(line);
    if (heading) current = heading[1].toLowerCase();
    else if (current) sections[current] = `${sections[current] ?? ''}${line}\n`;
  }
  const findings: Finding[] = [];
  const unparsed: string[] = [];
  for (const line of (sections.findings ?? '').split('\n')) {
    const m = FINDING.exec(line);
    if (!m && line.startsWith('- ')) unparsed.push(line);
    else if (m) {
      const cite = CITATION.exec(m[3]);
      findings.push({ index: findings.length + 1, label: m[1], anchor: m[2], path: cite?.[1], line: cite ? Number(cite[2]) : undefined, text: cite?.[3] ?? m[3] });
    } else if (findings.length && line.trim()) {
      findings.at(-1)!.text += `\n${line}`;
    }
  }
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

/** "Approve · 1 nit": the verdict, then finding counts by severity. */
export function summarizeReview(text: string): string {
  const r = parseReview(text);
  const counts = LABELS.flatMap((l) => {
    const n = r.findings.filter((f) => f.label === l).length;
    return n ? [`${n} ${n === 1 ? l : PLURAL[l]}`] : [];
  });
  return [VERDICT_NAMES[r.verdict ?? ''] ?? 'Review', counts.join(', ') || 'no findings'].join(' · ');
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

async function excerpt(api: ToolExecutionApi, repoDir: string, f: Finding, ctx: Context, tracked: Tracked = {}) {
  const none = { exists: false, excerpt: '', file: '', path: f.path };
  if (!f.path || !f.line) return none;
  let path = f.path;
  let read = await api.env!.readTextFile(`${repoDir}/${path}`, ctx);
  if (!read.ok) {
    tracked.files ??= await trackedFiles(api, repoDir, ctx);
    const found = resolveCitedPath(tracked.files, repoDir.split('/').pop()!, f.path);
    if (!found) return none;
    path = found;
    read = await api.env!.readTextFile(`${repoDir}/${path}`, ctx);
    if (!read.ok) return none;
  }
  const lines = read.value.split('\n');
  if (f.line < 1 || f.line > lines.length) return { ...none, file: read.value, path };
  const from = Math.max(1, f.line - EXCERPT_LINES);
  const text = lines.slice(from - 1, f.line + EXCERPT_LINES).map((l, i) => `${from + i}: ${l}`).join('\n');
  return { exists: true, excerpt: text, file: read.value, path };
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
    for (const f of r.findings) {
      const e = await excerpt(api, repoDir, f, ctx, tracked);
      const id = String(f.index);
      facts.findings.push({ id, cited: e.exists });
      findings.push({ id, blocking: f.label === 'blocking', text: `${f.label}${f.anchor ? ` (${f.anchor})` : ''}: ${e.path ?? '?'}:${f.line ?? '?'} ${f.text}`, citedCode: e.excerpt || '(the cited lines do not exist)' });
    }
    return {
      review: sealed.content,
      // Only an approve is asked whether it says why the change is safe; any other verdict leaves this empty, so it has no item.
      approve: r.verdict === 'approve' ? sealed.content : '',
      description: pr ? `${pr.title}\n\n${pr.body}` : '(unknown)',
      issues: pr ? pr.linkedIssues.map((i) => `## Linked issue #${i.number}: ${i.title}\n${i.body}`).join('\n\n') : '',
      diff,
      findings: JSON.stringify(findings),
      facts: JSON.stringify(facts),
    };
  };
}

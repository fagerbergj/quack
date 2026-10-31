import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import type { Evidence, Unit } from '@quack/judge';
import type { ArtifactKind, SealedArtifact } from '@quack/sdk';
import { PullRequestDoc } from './checkout.ts';

export const LABELS = ['blocking', 'suggestion', 'nit', 'question'] as const;
export const ANCHORS = ['defect', 'delivery', 'security', 'design', 'scope', 'tests'] as const;
export const VERDICTS = ['approve', 'comment', 'request_changes'] as const;

export type Finding = { index: number; label: string; anchor?: string; path?: string; line?: number; text: string };
export type ParsedReview = { sections: Record<string, string>; verdict?: string; findings: Finding[]; unparsed: string[] };

const FINDING = /^- (\w+)(?:\s*\((\w+)\))?:\s*(.*)$/;
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

/** Structure problems, all at once (ART-2); content problems are the judge's to find. */
export function validateReview(text: string): string[] {
  const r = parseReview(text);
  const problems = ['takeaway', 'verdict', 'findings'].filter((s) => !(s in r.sections)).map((s) => `missing a "## ${s[0].toUpperCase()}${s.slice(1)}" section`);
  if ('verdict' in r.sections && !VERDICTS.includes(r.verdict as never)) problems.push(`the verdict must be one of ${VERDICTS.join(', ')}`);
  for (const f of r.findings) if (!LABELS.includes(f.label as never)) problems.push(`finding ${f.index}: label must be one of ${LABELS.join(', ')}`);
  // A finding line the parser cannot read would be invisible to every gate, so it is a structure problem, not a skip.
  for (const line of r.unparsed) problems.push(`not a finding line ("- label (anchor): path:line — text"): ${line.slice(0, 80)}`);
  return problems;
}

export const reviewKind: ArtifactKind = { kind: 'github.review', path: 'artifacts/review.md', validate: validateReview };

const EXCERPT_LINES = 20;
const MAX_DIFF_CHARS = 120_000;

async function excerpt(api: ToolExecutionApi, repoDir: string, f: Finding, ctx: Context) {
  if (!f.path || !f.line) return { exists: false, excerpt: '', file: '' };
  const read = await api.env!.readTextFile(`${repoDir}/${f.path}`, ctx);
  if (!read.ok) return { exists: false, excerpt: '', file: '' };
  const lines = read.value.split('\n');
  if (f.line < 1 || f.line > lines.length) return { exists: false, excerpt: '', file: read.value };
  const from = Math.max(1, f.line - EXCERPT_LINES);
  const text = lines.slice(from - 1, f.line + EXCERPT_LINES).map((l, i) => `${from + i}: ${l}`).join('\n');
  return { exists: true, excerpt: text, file: read.value };
}

async function diffOf(api: ToolExecutionApi, repoDir: string, baseRef: string, ctx: Context): Promise<string> {
  let out = '';
  const r = await api.env!.exec(['git', '-C', repoDir, 'diff', `origin/${baseRef}...HEAD`], { onOutput: (t) => { out += t; } }, ctx);
  return r.ok ? out : '';
}

const truncated = (diff: string) => (diff.length > MAX_DIFF_CHARS ? `${diff.slice(0, MAX_DIFF_CHARS)}\n… diff truncated at ${MAX_DIFF_CHARS} characters` : diff);
const STATEMENT = /[{]|:=|\s=\s|==|=>|\breturn\b/;
const squash = (s: string) => s.replace(/\s+/g, ' ').trim();

/** Inline code a finding's prose quotes as existing code: statement-like spans, split at elisions; fenced proposals excluded. */
export function quotedCode(text: string): string[][] {
  const prose = text.replace(/```[\s\S]*?```/g, '');
  return [...prose.matchAll(/`([^`\n]+)`/g)]
    .map((m) => m[1])
    .filter((s) => s.length >= 12 && STATEMENT.test(s))
    .map((s) => s.split(/\.\.\.|…/).map(squash).filter((p) => p.length >= 6));
}

// A quote must exist somewhere: the cited file, the change (old or new side), or anywhere else in the repository.
async function misquotes(api: ToolExecutionApi, repoDir: string, f: Finding, file: string, diff: string, ctx: Context): Promise<string[]> {
  const haystack = squash(file) + '\n' + squash(diff.split('\n').map((l) => l.replace(/^[+\- ]/, '')).join('\n'));
  const missing: string[] = [];
  for (const parts of quotedCode(f.text)) {
    for (const p of parts) {
      if (haystack.includes(p)) continue;
      const r = await api.env!.exec(['git', '-C', repoDir, 'grep', '-q', '-F', '-e', p, 'HEAD', '--'], {}, ctx);
      if (!(r.ok && r.value.exitCode === 0)) missing.push(p);
    }
  }
  return missing;
}

// Evidence scopes: the review alone (small), the review with its change (large), and each finding with its cited code.
export function reviewEvidence(repoDir = 'repo') {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Evidence> => {
    const r = parseReview(sealed.content);
    const pr = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr;
    const change = pr
      ? { description: `${pr.title}\n\n${pr.body}`, ci: pr.checks.map((c) => `${c.name}=${c.conclusion ?? c.status}`).join(', '), linkedIssues: pr.linkedIssues.map((i) => `#${i.number} ${i.title}\n${i.body}`).join('\n\n'), diff: await diffOf(api, repoDir, pr.baseRef, ctx) }
      : { description: '(unknown)', ci: '(unknown)', linkedIssues: '', diff: '' };
    const diff = change.diff;
    change.diff = pr ? truncated(diff) || '(diff unavailable)' : '(no checkout)';
    const findings: Array<Unit & { blocking: boolean; cited: boolean; misquotes: string[] }> = [];
    for (const f of r.findings) {
      const e = await excerpt(api, repoDir, f, ctx);
      const wrong = pr ? await misquotes(api, repoDir, f, e.file, diff, ctx) : [];
      findings.push({ id: `finding-${f.index}`, blocking: f.label === 'blocking', cited: e.exists, misquotes: wrong, state: { finding: `${f.label}${f.anchor ? ` (${f.anchor})` : ''}: ${f.path ?? '?'}:${f.line ?? '?'} ${f.text}`, citedCode: e.excerpt || '(the cited lines do not exist)' } });
    }
    return {
      review: [{ id: 'review', state: { review: sealed.content } }],
      change: [{ id: 'change', state: { review: sealed.content, ...change } }],
      finding: findings,
      blocking: findings.filter((f) => f.blocking),
      lower: findings.filter((f) => !f.blocking),
    };
  };
}

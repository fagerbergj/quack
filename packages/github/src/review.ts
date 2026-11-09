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

export const reviewKind: ArtifactKind = { kind: 'github.review', path: 'artifacts/review.md', validate: validateReview, outcome: (text) => parseReview(text).verdict, summarize: summarizeReview, basis: async (api, ctx) => (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr?.headSha };

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

/** Where the repo's installed JS dependencies would be: node_modules beside each tracked package.json, at any depth. */
export async function dependencyDirs(api: ToolExecutionApi, repoDir: string, ctx: Context): Promise<string[]> {
  const manifests = (await gitRead(api, repoDir, ['ls-files', '--', ':(glob)**/package.json'], ctx)).split('\n').filter(Boolean);
  return manifests.map((m) => `${repoDir}/${m.slice(0, -'package.json'.length)}node_modules`);
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

const KEYWORDS = new Set(['true', 'false', 'null', 'undefined', 'void', 'this', 'new', 'typeof', 'instanceof', 'return', 'const', 'let', 'var', 'function', 'class', 'async', 'await', 'string', 'number', 'boolean']);
const MAX_NAMES = 8;
const DEP_GLOBS = ['--include=*.js', '--include=*.mjs', '--include=*.cjs', '--include=*.ts'];
// Each search is bounded; one that runs out cannot show a name invented, so it counts as found.
const SEARCH_SECONDS = 20;

/** Names a span claims exist: members, callees and object keys. Bare locals and string contents are proposals. */
export function claimedNames(span: string): string[] {
  const code = span.replace(/(["'`])(?:\\.|(?!\1).)*\1/g, '""');
  const names = new Set<string>();
  for (const m of code.matchAll(/([A-Za-z_$][\w$]*)\s*([(:,}])?/g)) {
    const before = code.slice(0, m.index);
    const inBraces = before.split('{').length > before.split('}').length;
    // The 3-character floor skips locals like i, x, ok that every repo contains anyway.
    if (m[1].length >= 3 && !KEYWORDS.has(m[1]) && (before.endsWith('.') || m[2] === '(' || inBraces)) names.add(m[1]);
  }
  return [...names].slice(0, MAX_NAMES);
}

// A span is invented only when a name it claims exists nowhere: the cited file, the change, the repo or installed dependencies.
export async function misquotes(api: ToolExecutionApi, repoDir: string, f: Finding, file: string, diff: string, ctx: Context, known = new Map<string, boolean>(), deps = [`${repoDir}/node_modules`]): Promise<string[]> {
  const haystack = squash(file) + '\n' + squash(diff.split('\n').map((l) => l.replace(/^[+\- ]/, '')).join('\n'));
  const grep = async (argv: string[]) => {
    const r = await api.env!.exec(argv, { timeout: SEARCH_SECONDS }, ctx);
    return r.ok ? r.value.exitCode === 0 : r.error.code === 'timeout';
  };
  const inDeps = (id: string) => deps.length > 0 && grep(['grep', '-rqsw', '-F', ...DEP_GLOBS, '-e', id, ...deps]);
  const exists = async (id: string) => {
    if (new RegExp(`(?<![\\w$])${id.replace(/\$/g, '\\$')}(?![\\w$])`).test(haystack)) return true;
    if (!known.has(id)) known.set(id, await grep(['git', '-C', repoDir, 'grep', '-q', '-w', '-F', '-e', id, 'HEAD', '--']) || await inDeps(id));
    return known.get(id)!;
  };
  const invented: string[] = [];
  for (const parts of quotedCode(f.text)) {
    for (const p of parts) {
      if (haystack.includes(p)) continue;
      for (const id of claimedNames(p)) if (!invented.includes(id) && !(await exists(id))) invented.push(id);
    }
  }
  return invented;
}

/** What code found about each finding, for the gates: whether its citation exists and which quoted names exist nowhere. */
export type ReviewFacts = { ci: string; findings: Array<{ id: string; cited: boolean; misquotes: string[] }> };

// The named texts the declaration reads: the review, the change it reviewed, each finding with its cited code, and the facts code found.
export function reviewInputs(repoDir = 'repo') {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Inputs> => {
    const r = parseReview(sealed.content);
    const pr = (await api.snapshot(PullRequestDoc, api.conversationId, ctx))?.pr;
    const diff = pr ? await diffOf(api, repoDir, pr.baseRef, ctx) : '';
    const known = new Map<string, boolean>();
    const tracked: Tracked = {};
    const deps = pr && r.findings.length ? await dependencyDirs(api, repoDir, ctx) : [];
    const facts: ReviewFacts = { ci: pr ? pr.checks.map((c) => `${c.name}=${c.conclusion ?? c.status}`).join(', ') : '(unknown)', findings: [] };
    const findings = [];
    for (const f of r.findings) {
      const e = await excerpt(api, repoDir, f, ctx, tracked);
      const id = String(f.index);
      facts.findings.push({ id, cited: e.exists, misquotes: pr ? await misquotes(api, repoDir, f, e.file, diff, ctx, known, deps) : [] });
      findings.push({ id, blocking: f.label === 'blocking', text: `${f.label}${f.anchor ? ` (${f.anchor})` : ''}: ${e.path ?? '?'}:${f.line ?? '?'} ${f.text}`, citedCode: e.excerpt || '(the cited lines do not exist)' });
    }
    return {
      review: sealed.content,
      // Only an approve is asked whether it says why the change is safe; any other verdict leaves this empty, so it has no item.
      approve: r.verdict === 'approve' ? sealed.content : '',
      description: pr ? `${pr.title}\n\n${pr.body}` : '(unknown)',
      issues: pr ? pr.linkedIssues.map((i) => `Linked issue #${i.number} ${i.title}\n${i.body}`).join('\n\n') : '',
      diff,
      findings: JSON.stringify(findings),
      facts: JSON.stringify(facts),
    };
  };
}

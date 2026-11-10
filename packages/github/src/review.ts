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
const MAX_DIFF_CHARS = 120_000;

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

/** A language whose dependencies and standard library are both searchable here: its source extensions and where they are. */
export type DepSearch = { exts: string[]; dirs: string[] };

// Each search is bounded; one that runs out cannot show a name invented, so it counts as found.
const SEARCH_SECONDS = 20;

// A bounded command's stdout, and whether it exited 0 in time.
async function run(api: ToolExecutionApi, argv: string[], ctx: Context): Promise<{ ok: boolean; out: string }> {
  let out = '';
  const r = await api.env!.exec(argv, { timeout: SEARCH_SECONDS, onOutput: (t, _c, info) => { if (info.stream === 'stdout') out += t; } }, ctx);
  return { ok: r.ok && r.value.exitCode === 0, out };
}
const lines = (s: string) => s.split('\n').filter(Boolean);
const existing = async (api: ToolExecutionApi, paths: string[], ctx: Context) => (paths.length ? lines((await run(api, ['ls', '-d', '--', ...paths], ctx)).out).filter((p) => paths.includes(p)) : []);
// grep -r already covers a directory's subdirectories.
const outermost = (dirs: string[]) => [...new Set(dirs)].filter((d, _, all) => !all.some((o) => d.startsWith(`${o}/`)));

async function jsSearch(api: ToolExecutionApi, dirs: string[], ctx: Context): Promise<DepSearch | undefined> {
  const modules = dirs.map((d) => `${d}node_modules`);
  const found = await existing(api, [...modules, ...modules.map((m) => `${m}/typescript/lib`)], ctx);
  // The JS standard library exists as source only in TypeScript's lib.*.d.ts.
  return found.some((p) => p.endsWith('/typescript/lib')) ? { exts: ['.js', '.mjs', '.cjs', '.jsx', '.ts', '.mts', '.cts', '.tsx'], dirs: modules.filter((m) => found.includes(m)) } : undefined;
}

async function goSearch(api: ToolExecutionApi, dirs: string[], ctx: Context): Promise<DepSearch | undefined> {
  const found: string[] = [];
  for (const d of dirs) {
    // Every package the module builds with, stdlib and builtins included; offline, so a module not installed fails it.
    const r = await run(api, ['env', 'GOPROXY=off', 'GOTOOLCHAIN=local', 'go', '-C', d, 'list', '-deps', '-test', '-f', '{{.Dir}}', 'builtin', './...'], ctx);
    if (!r.ok) return undefined;
    found.push(...lines(r.out));
  }
  return found.length ? { exts: ['.go'], dirs: outermost(found) } : undefined;
}

const PY_PATHS = 'import sysconfig as s; print(*{s.get_path(k) for k in ("stdlib", "purelib", "platlib")}, sep="\\n")';
async function pySearch(api: ToolExecutionApi, dirs: string[], ctx: Context): Promise<DepSearch | undefined> {
  const found: string[] = [];
  for (const py of await existing(api, dirs.flatMap((d) => [`${d}.venv/bin/python`, `${d}venv/bin/python`]), ctx)) {
    const r = await run(api, [py, '-I', '-c', PY_PATHS], ctx);
    if (r.ok) found.push(...lines(r.out));
  }
  return found.length ? { exts: ['.py', '.pyi'], dirs: outermost(found) } : undefined;
}

// Languages are found by their manifests; one not listed is never searchable, so no name in it can be shown invented.
const LANGUAGES = [
  { manifest: /^package\.json$/, search: jsSearch },
  { manifest: /^go\.mod$/, search: goSearch },
  { manifest: /^(pyproject\.toml|setup\.py|setup\.cfg|Pipfile|requirements.*\.txt)$/, search: pySearch },
];

/** The repo's languages whose installed dependencies and standard library can be searched in this sandbox. */
export async function dependencyDirs(api: ToolExecutionApi, repoDir: string, ctx: Context): Promise<DepSearch[]> {
  const files = await trackedFiles(api, repoDir, ctx);
  const found: DepSearch[] = [];
  for (const { manifest, search } of LANGUAGES) {
    const dirs = [...new Set(files.filter((f) => manifest.test(f.split('/').pop()!)).map((f) => `${repoDir}/${f.replace(/[^/]*$/, '')}`))];
    const s = dirs.length ? await search(api, dirs, ctx) : undefined;
    if (s) found.push(s);
  }
  return found;
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

const MAX_NAMES = 8;

/** Names a span claims exist: members, callees and object keys. Bare locals and string contents are proposals. */
export function claimedNames(span: string): string[] {
  const code = span.replace(/(["'`])(?:\\.|(?!\1).)*\1/g, '""');
  const names = new Set<string>();
  for (const m of code.matchAll(/([A-Za-z_$][\w$]*)\s*([(:,}])?/g)) {
    const before = code.slice(0, m.index);
    const inBraces = before.split('{').length > before.split('}').length;
    // The 3-character floor skips locals like i, x, ok that every repo contains anyway.
    if (m[1].length >= 3 && (before.endsWith('.') || m[2] === '(' || inBraces)) names.add(m[1]);
  }
  return [...names];
}

// A span is invented only when a name it claims exists nowhere: the cited file, the change, the repo, or its language's dependencies and stdlib.
export async function misquotes(api: ToolExecutionApi, repoDir: string, f: Finding, file: string, diff: string, ctx: Context, known: Map<string, boolean>, deps: DepSearch[]): Promise<string[]> {
  // Where the cited file's language can't be searched, a name not found is unknown, and unknown is not invented.
  const lang = deps.find((d) => d.exts.some((e) => f.path?.endsWith(e)));
  if (!lang) return [];
  const haystack = squash(file) + '\n' + squash(diff.split('\n').map((l) => l.replace(/^[+\- ]/, '')).join('\n'));
  const grep = async (argv: string[]) => {
    const r = await api.env!.exec(argv, { timeout: SEARCH_SECONDS }, ctx);
    return r.ok ? r.value.exitCode === 0 : r.error.code === 'timeout';
  };
  const inDeps = (id: string) => grep(['grep', '-rqsw', '-F', ...lang.exts.map((e) => `--include=*${e}`), '-e', id, ...lang.dirs]);
  const inText = (id: string) => new RegExp(`(?<![\\w$])${id.replace(/\$/g, '\\$')}(?![\\w$])`).test(haystack);
  const exists = async (id: string) => {
    const key = `${lang.exts[0]} ${id}`;
    if (!known.has(key)) known.set(key, await grep(['git', '-C', repoDir, 'grep', '-q', '-w', '-F', '-e', id, 'HEAD', '--']) || await inDeps(id));
    return known.get(key)!;
  };
  const invented: string[] = [];
  for (const parts of quotedCode(f.text)) {
    for (const p of parts) {
      if (haystack.includes(p)) continue;
      // Keywords and builtins need no list: the cited file or the stdlib source holds them.
      for (const id of claimedNames(p).filter((n) => !inText(n)).slice(0, MAX_NAMES)) if (!invented.includes(id) && !(await exists(id))) invented.push(id);
    }
  }
  return invented;
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
    change.diff = pr ? truncated(diff) || '(empty diff)' : '(no checkout)';
    const known = new Map<string, boolean>();
    const tracked: Tracked = {};
    const deps = pr && r.findings.length ? await dependencyDirs(api, repoDir, ctx) : [];
    const findings: Array<Unit & { blocking: boolean; cited: boolean; misquotes: string[] }> = [];
    for (const f of r.findings) {
      const e = await excerpt(api, repoDir, f, ctx, tracked);
      const wrong = pr ? await misquotes(api, repoDir, f, e.file, diff, ctx, known, deps) : [];
      findings.push({ id: `finding-${f.index}`, blocking: f.label === 'blocking', cited: e.exists, misquotes: wrong, state: { finding: `${f.label}${f.anchor ? ` (${f.anchor})` : ''}: ${e.path ?? '?'}:${f.line ?? '?'} ${f.text}`, citedCode: e.excerpt || '(the cited lines do not exist)' } });
    }
    const changeUnit = { id: 'change', state: { review: sealed.content, ...change } };
    return {
      review: [{ id: 'review', whole: true, state: { review: sealed.content } }],
      change: [changeUnit],
      // Judged on the review alone: whether an approve says why the change is safe, not whether the diff is defect-free.
      approve: r.verdict === 'approve' ? [{ id: 'approve', state: { review: sealed.content } }] : [],
      finding: findings,
      blocking: findings.filter((f) => f.blocking),
      lower: findings.filter((f) => !f.blocking),
    };
  };
}

import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';

export type Run = { command: string; failed: boolean };

const PR = 'artifacts/pr.md';
type Tx = Parameters<Parameters<ToolExecutionApi['commit']>[0]>[0];

// shortcut: a backticked span with a space counts as a command and matching is by normalized substring, so prose like `a = 3` can false-flag and a command run inside a script is missed; upgrade by having the agent list its runs in a structured block.
const norm = (s: string) => s.replace(/['"]/g, '').replace(/(^|&&|;)\s*cd\s+\S+\s*&&/g, '$1').replace(/\s+/g, ' ').trim();

// The whole line around index i; the last line may have no newline after it.
const lineAt = (t: string, i: number) => { const end = t.indexOf('\n', i); return t.slice(t.lastIndexOf('\n', i) + 1, end < 0 ? undefined : end); };
const spans = (text: string) => [...text.matchAll(/`([^`\n]+)`/g)].map((m) => ({ code: m[1].trim(), line: lineAt(text, m.index) }));

/** Backticked commands in the "How verified" part of the PR text, with the line that claims them. */
export function claimedCommands(text: string) {
  const at = text.search(/^\W*(how )?verified\b/im);
  if (at < 0) return [];
  const section = text.slice(at).split(/\n#{1,6} /)[0];
  return spans(section).filter((s) => /^[\w./-]+ \S/.test(s.code));
}

/** Short hex ids in backticks anywhere in the PR text, which name commits. */
export const claimedShas = (text: string) => spans(text).map((s) => s.code).filter((c) => /^(?=.*\d)[0-9a-f]{7,40}$/.test(c));

/** Every bash call in this session with whether it exited non-zero, read from the transcript. */
export async function sessionRuns(api: ToolExecutionApi, ctx: Context): Promise<Run[]> {
  const commands = new Map<string, string>();
  const runs: Run[] = [];
  let cursor: Parameters<Tx['scanEntries']>[2];
  do {
    const page = await api.commit((tx) => tx.scanEntries({ conversationId: api.conversationId, order: 'ascending' }, 200, cursor), ctx);
    for (const m of page.items.flatMap((e) => e.model ?? [])) {
      if (m.role === 'assistant') for (const c of m.content) if (c.type === 'toolCall' && c.name === 'bash') commands.set(c.id, String((c.arguments as { command?: unknown }).command ?? ''));
      if (m.role === 'toolResult' && commands.has(m.toolCallId)) runs.push({ command: commands.get(m.toolCallId)!, failed: m.isError });
    }
    cursor = page.next;
  } while (cursor);
  return runs;
}

/** Each claim the session's runs do not back; a failed run backs only a line that says it failed. */
export function unbackedClaims(text: string, runs: Run[], commits: string[]): string[] {
  const found = claimedCommands(text).flatMap(({ code, line }) => {
    const hits = runs.filter((r) => norm(r.command).includes(norm(code)));
    if (!hits.length) return [`${PR} says you ran \`${code}\`, but this session never ran it`];
    if (hits.every((r) => r.failed) && !/\bfail|\bred\b|\berror|\bbroke/i.test(line)) return [`${PR} says \`${code}\` passed, but every run of it exited non-zero`];
    return [];
  });
  const missing = claimedShas(text).filter((s) => !commits.some((c) => c.startsWith(s)));
  return [...found, ...missing.map((s) => `${PR} names commit ${s}, which is not among the commits made since checkout`)];
}

/** What the delivered commits changed: each path's `git diff --name-status` letter, and every path at the base or at HEAD. */
export type DiffFacts = { changed: Map<string, string>; paths: string[] };

const SHA = /^(?=.*\d)[0-9a-f]{7,40}$/;
const PATH = /^[\w@.-]+(\/[\w@.-]+)*(\/|\.[A-Za-z]{1,5})$/;
const CODE = /^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$/;
const proseFree = (text: string) => text.replace(/^```[\s\S]*?^```/gm, '');

// A span names a path when it has a slash and ends in an extension or a slash; a code name when it reads as camelCase, snake_case, Capitalized, dotted or called.
function nameOf(span: string): { path?: string; code?: string } {
  const s = span.replace(/:\d+(-\d+)?$/, '').replace(/^\.\//, '');
  if (s.includes('/')) return PATH.test(s) ? { path: s } : {};
  const bare = s.replace(/\(\)$/, '');
  if (!CODE.test(bare) || SHA.test(bare)) return {};
  const last = bare.split('.').at(-1)!;
  return last.length > 2 && (bare !== s || bare.includes('.') || /[a-z][A-Z]|_|^[A-Z]/.test(last)) ? { code: last } : {};
}

/** The code names the PR text puts in backticks, which the caller looks up in the repository. */
export const namedCode = (text: string) => [...new Set(spans(proseFree(text)).map((s) => nameOf(s.code).code).filter((c): c is string => !!c))];

const matches = (paths: Iterable<string>, p: string) => [...paths].filter((f) => f === p || f.endsWith(`/${p}`) || (p.endsWith('/') && (f.startsWith(p) || f.includes(`/${p}`))));

// Phrasings that tie a named path to what the diff did with it.
const SAYS: { re: RegExp; ok: (statuses: string[]) => boolean; claim: string }[] = [
  { re: /\bnew (?:file )?`([^`]+)`|`([^`]+)` \(new\)/gi, ok: (s) => s.includes('A'), claim: 'is new, but the diff does not add it' },
  { re: /\b(?:deleted?|removed?) (?:the )?(?:file )?`([^`]+)`|`([^`]+)` (?:is |was )?(?:deleted|removed)\b/gi, ok: (s) => s.includes('D'), claim: 'was deleted, but the diff does not delete it' },
  { re: /\b(?:changes?|edit(?:s|ed)?|updated?|modified|rewritten|added|touched) (?:in |to )?`([^`]+)`/gi, ok: (s) => s.length > 0, claim: 'changed, but the diff does not touch it' },
  { re: /`([^`]+)` (?:is |stays |remains )?(?:unchanged|untouched)\b|\bno changes? to `([^`]+)`/gi, ok: (s) => !s.length, claim: 'is unchanged, but the diff touches it' },
];

// A "no", "not" or "never" earlier in the clause, outside backticks, flips what a phrase claims, so the phrase is skipped.
const negated = (text: string, at: number) => /\b(?:no|not|never)\b|n't\b/i.test(text.slice(0, at).replace(/`[^`\n]*`/g, '').split(/[.;:!?\n]/).at(-1)!);

function pathClaims(text: string, d: DiffFacts) {
  return SAYS.flatMap(({ re, ok, claim }) => [...text.matchAll(re)].flatMap((m) => {
    const path = nameOf(m[1] ?? m[2]).path;
    if (!path || !matches(d.paths, path).length || negated(text, m.index)) return [];
    return ok(matches(d.changed.keys(), path).map((f) => d.changed.get(f)![0])) ? [] : [`${PR} says \`${path}\` ${claim}`];
  }));
}

const LANGS: Record<string, RegExp> = { go: /\.go$/, typescript: /\.tsx?$/, javascript: /\.[cm]?jsx?$/, python: /\.py$/ };

function scopeClaims(text: string, changed: string[]) {
  const found: string[] = [];
  const notDocs = changed.find((f) => !/\.md$/.test(f));
  const docsOnly = [...text.matchAll(/\b(?:docs?|documentation)-only\b/gi)].some((m) => !negated(text, m.index));
  if (notDocs && docsOnly) found.push(`${PR} says the change is docs-only, but the diff touches ${notDocs}`);
  // Only an unqualified "no Go changes" that ends its clause; "no Go changes outside X" is a narrower claim.
  for (const m of text.matchAll(/\bno (go|typescript|javascript|python)(?: code)? changes?(?=\s*(?:[.,;:)]|$))/gim)) {
    const hit = changed.find((f) => LANGS[m[1].toLowerCase()].test(f));
    if (hit) found.push(`${PR} says there are no ${m[1]} changes, but the diff touches ${hit}`);
  }
  return found;
}

const CUE = /\b(?:add(?:s|ed|ing)?|new|introduc(?:e|es|ed|ing)|renam(?:e|es|ed|ing)|creat(?:e|es|ed|ing)|exports?|exported|remov(?:e|es|ed|ing)|delet(?:e|es|ed|ing)|drop(?:s|ped)?)\b/i;

// A path counts only under one of the repo's top directories, and a code name only in a paragraph or list item that says something was added or removed, so another repo's files, workspace files and dependency APIs cited as context are not flagged.
function ghosts(body: string, d: DiffFacts, found: Set<string>) {
  const roots = new Set(d.paths.filter((p) => p.includes('/')).map((p) => p.split('/')[0]));
  return [...new Set(body.split(/\n\s*\n|\n(?=\s*(?:[-*+]|\d+\.)\s)/).flatMap((block) => spans(block).flatMap((s) => {
    const { path, code } = nameOf(s.code);
    if (path) return roots.has(path.split('/')[0]) && !matches(d.paths, path).length ? [path] : [];
    return code && CUE.test(block) && !found.has(code) ? [code] : [];
  })))];
}

/** Each claim about the diff that the diff contradicts: a named path, or a code name said to be added or removed, that exists nowhere, a file said to be new, deleted, changed or unchanged that is not, and a docs-only or no-Go scope the diff breaks. `found` holds the code names a search of the base and HEAD trees found. */
export function diffClaims(text: string, d: DiffFacts, found: Set<string>): string[] {
  const body = proseFree(text);
  const missing = ghosts(body, d, found).map((g) => `${PR} names \`${g}\`, which is in neither the diff nor the repository`);
  return [...missing, ...pathClaims(body, d), ...scopeClaims(body, [...d.changed.keys()])];
}

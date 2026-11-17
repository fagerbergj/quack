import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { countFindings, diffOf, type Finding, type ParsedReview, parseReview, resolveCitedPath, trackedFiles } from './review.ts';

export type InlineComment = { path: string; line: number; body: string };

/** New-file lines a diff shows (added and context), per path: the lines GitHub accepts an inline comment on. */
export function commentableLines(diff: string): Map<string, Set<number>> {
  const out = new Map<string, Set<number>>();
  let lines: Set<number> | undefined;
  let inHunk = false;
  let n = 0;
  for (const line of diff.split('\n')) {
    if (line.startsWith('diff --git ')) { lines = undefined; inHunk = false; continue; }
    const file = inHunk ? null : /^\+\+\+ b\/(.+)$/.exec(line);
    if (file) out.set(file[1], (lines = new Set()));
    const hunk = /^@@ -\d+(?:,\d+)? \+(\d+)/.exec(line);
    if (hunk) { inHunk = true; n = Number(hunk[1]); } else if (inHunk && lines && (line[0] === '+' || line[0] === ' ')) lines.add(n++);
  }
  return out;
}

const SENTENCE = /^.*?[.!?](?=\s|$)/;

function firstSentence(text: string): string {
  const first = text.split('\n')[0];
  return SENTENCE.exec(first)?.[0] ?? first;
}

const keyOf = (f: Finding) => `${f.label}|${f.path}|${f.line}|${f.text}`;
const unanchored = (f: Finding) => `- ${f.label}${f.anchor ? ` (${f.anchor})` : ''}: ${f.path ? `${f.path}:${f.line} — ` : ''}${f.text}`;
const EVIDENCE_SKIP = new Set(['takeaway', 'verdict', 'findings']);

type Rendered = { inline: Finding[]; unposted: Finding[]; open: number; resolved?: number };

// GitHub already shows the verdict (the review state) and each inline finding, so the body keeps the takeaway, what is not inline, and the evidence collapsed.
function render(r: ParsedReview, x: Rendered): string {
  const out = [r.sections.takeaway?.trim() || `Verdict: ${r.verdict}.`];
  const notes = [x.inline.length ? `${countFindings(x.inline)} inline.` : '', x.resolved === undefined ? '' : `Since the last review: ${x.resolved} no longer raised, ${x.open} still open in their threads.`].filter(Boolean);
  if (notes.length) out.push(notes.join(' '));
  if (x.unposted.length) out.push(['Not anchored to a diff line:', ...x.unposted.map(unanchored)].join('\n'));
  const evidence = Object.entries(r.sections).filter(([k, v]) => !EVIDENCE_SKIP.has(k) && v.trim()).map(([k, v]) => `**${k[0].toUpperCase()}${k.slice(1)}**\n\n${v.trim()}`);
  if (evidence.length) out.push(`<details><summary>What I checked</summary>\n\n${evidence.join('\n\n')}\n\n</details>`);
  return out.join('\n\n');
}

// In-diff findings become inline comments unless `prior` posted them unchanged; `fallbackBody` lists every finding, for when GitHub refuses the comments.
export function splitReview(content: string, diff: string, files: string[], repoName: string, prior?: string): { body: string; fallbackBody: string; comments: InlineComment[] } {
  const shown = commentableLines(diff);
  const r = parseReview(content);
  const before = prior === undefined ? undefined : parseReview(prior).findings.map(keyOf);
  const comments: InlineComment[] = [];
  const inline: Finding[] = [];
  const unposted: Finding[] = [];
  let open = 0;
  for (const f of r.findings) {
    const path = f.path && f.line ? resolveCitedPath(files, repoName, f.path) : undefined;
    if (before?.includes(keyOf(f))) { open++; continue; }
    if (!path || !shown.get(path)?.has(f.line!)) { unposted.push(f); continue; }
    const first = firstSentence(f.text);
    const rest = f.text.slice(first.length).trim();
    inline.push(f);
    comments.push({ path, line: f.line!, body: `**${f.label}${f.anchor ? ` (${f.anchor})` : ''}**: ${first}${rest ? `\n\n${rest}` : ''}` });
  }
  const resolved = before && before.filter((k) => !r.findings.some((f) => keyOf(f) === k)).length;
  return { body: render(r, { inline, unposted, open, resolved }), fallbackBody: render(r, { inline: [], unposted: [...inline, ...unposted], open, resolved }), comments };
}

/** The review as it posts: in-diff findings inline, judged against the checkout's own diff; `prior` is the session's last posted review. */
export async function postable(api: ToolExecutionApi, repoDir: string, baseRef: string, content: string, ctx: Context, prior?: string) {
  const diff = await diffOf(api, repoDir, baseRef, ctx);
  return splitReview(content, diff, diff ? await trackedFiles(api, repoDir, ctx) : [], repoDir.split('/').pop()!, prior);
}

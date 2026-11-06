import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { diffOf, FINDING, parseReview, resolveCitedPath, trackedFiles } from './review.ts';

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
const MAX_TITLE = 120;

function firstSentence(text: string): string {
  const first = text.split('\n')[0];
  return SENTENCE.exec(first)?.[0] ?? first;
}

const shorten = (s: string) => (s.length > MAX_TITLE ? `${s.slice(0, MAX_TITLE - 1)}…` : s);

// The finding each line of the Findings section belongs to (0 elsewhere), following parseReview's continuation rule, and where the section ends.
function lineOwners(lines: string[]) {
  let section = '';
  let current = 0;
  let count = 0;
  let end = -1;
  const owner = lines.map((line, i) => {
    const heading = /^## (\w+)/.exec(line);
    if (heading) { section = heading[1].toLowerCase(); current = 0; return 0; }
    if (section !== 'findings') return 0;
    if (line.trim()) end = i;
    if (FINDING.test(line)) return (current = ++count);
    return line.startsWith('- ') ? 0 : current;
  });
  return { owner, end };
}

/** Findings whose cited line is in the diff become inline comments; the body keeps the rest and lists those by title. */
export function splitReview(content: string, diff: string, files: string[], repoName: string): { body: string; comments: InlineComment[] } {
  const shown = commentableLines(diff);
  const inline = new Map<number, { label: string; title: string }>();
  const comments: InlineComment[] = [];
  for (const f of parseReview(content).findings) {
    const path = f.path && f.line ? resolveCitedPath(files, repoName, f.path) : undefined;
    if (!path || !shown.get(path)?.has(f.line!)) continue;
    const first = firstSentence(f.text);
    const rest = f.text.slice(first.length).trim();
    inline.set(f.index, { label: f.label, title: shorten(first) });
    comments.push({ path, line: f.line!, body: `**${f.label}${f.anchor ? ` (${f.anchor})` : ''}**: ${first}${rest ? `\n\n${rest}` : ''}` });
  }
  if (!comments.length) return { body: content, comments };
  const lines = content.split('\n');
  const { owner, end } = lineOwners(lines);
  const listed = ['', 'Posted inline on the diff:', ...[...inline.values()].map((f) => `- ${f.label}: ${f.title}`)];
  const kept = lines.flatMap((line, i) => [...(inline.has(owner[i]) ? [] : [line]), ...(i === end ? listed : [])]);
  return { body: kept.join('\n'), comments };
}

/** The review as it posts: in-diff findings inline, judged against the checkout's own diff. */
export async function postable(api: ToolExecutionApi, repoDir: string, baseRef: string, content: string, ctx: Context) {
  const diff = await diffOf(api, repoDir, baseRef, ctx);
  return diff ? splitReview(content, diff, await trackedFiles(api, repoDir, ctx), repoDir.split('/').pop()!) : { body: content, comments: [] };
}

// No node: imports, so the web bundle or an artifact route can render a delivered report from sealed content and its verdict.
import { feedback, type CheckResult } from '@quack/judge/rubric';

const CITATION = /\[(s\d+)\]/g;

export const citedIds = (text: string) => [...new Set([...text.matchAll(CITATION)].map((m) => m[1]))];

type Verdict = { passed: boolean; limitReached: boolean; checks: CheckResult[] };

// What a failing check's marks mean on a line: source ids on lines citing them, a finding's first line, or quoted text the line contains.
const NOTES: Record<string, (line: string, marks: Set<string>) => string | undefined> = {
  cites_fetched_sources: (l, m) => {
    const bad = citedIds(l).filter((id) => m.has(id));
    return bad.length ? `not checked: ${bad.join(', ')} never fetched` : undefined;
  },
  findings_cited: (l, m) => (m.has(l.trim()) ? 'no source cited' : undefined),
  quotes_exist: (l, m) => ([...m].some((q) => l.includes(q)) ? 'quote not found in the cited source' : undefined),
};

/** The report as delivered (RES-4, ART-4): passed unchanged; released at the revision limit with a banner and a note on each line a failing check marked; undefined while still being revised. */
export function deliveredReport(content: string, v: Verdict | null | undefined): string | undefined {
  if (!v || !(v.passed || v.limitReached)) return undefined;
  if (v.passed) return content;
  // Marks come from what the check saw, not from the session's later state, so they always agree with the banner.
  const failing = v.checks.filter((c) => c.met !== null && c.met < 1 && c.marks?.length && NOTES[c.id]);
  const lines = content.split('\n').map((l) => {
    const notes = failing.map((c) => NOTES[c.id](l, new Set(c.marks))).filter(Boolean);
    return notes.length ? `${l} (${notes.join('; ')})` : l;
  });
  const failed = feedback(v.checks).map((l) => `> - ${l}`).join('\n');
  return `> **Not fully checked.** These checks were still failing when this version was released:\n${failed}\n\n${lines.join('\n')}`;
}

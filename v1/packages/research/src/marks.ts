// No node: imports, so the web bundle or an artifact route can render a delivered report from sealed content and its verdict.
import { feedback, type CheckResult } from '@quack/judge/rubric';

const CITATION = /\[(s\d+)\]/g;

export const citedIds = (text: string) => [...new Set([...text.matchAll(CITATION)].map((m) => m[1]))];

type Verdict = { passed: boolean; limitReached: boolean; checks: CheckResult[] };

/** The report as delivered (RES-4, ART-4): passed unchanged; released at a revision or turn limit with a banner and a mark on each line citing a never-fetched source; undefined while still being revised. */
export function deliveredReport(content: string, v: Verdict | null | undefined): string | undefined {
  if (!v || !(v.passed || v.limitReached)) return undefined;
  if (v.passed) return content;
  // Marks come from what the check saw, not from the session's later state, so they always agree with the banner.
  const blamed = new Set(v.checks.flatMap((c) => (c.met !== null && c.met < 1 ? c.marks ?? [] : [])));
  const lines = content.split('\n').map((l) => {
    const bad = citedIds(l).filter((id) => blamed.has(id));
    return bad.length ? `${l} (not checked: ${bad.join(', ')} never fetched)` : l;
  });
  const failed = feedback(v.checks).map((l) => `> - ${l}`).join('\n');
  return `> **Not fully checked.** These checks were still failing when this version was released:\n${failed}\n\n${lines.join('\n')}`;
}

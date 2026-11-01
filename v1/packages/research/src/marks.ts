// No node: imports, so the web bundle or an artifact route can render a delivered report from sealed content and its verdict.
import { feedback, type CheckResult } from '@quack/judge/rubric';

const CITATION = /\[(s\d+)\]/g;

export const citedIds = (text: string) => [...new Set([...text.matchAll(CITATION)].map((m) => m[1]))];

type Verdict = { passed: boolean; limitReached: boolean; checks: CheckResult[] };

/** The report as delivered (RES-4, ART-4): passed unchanged; released at the limit with a banner and a mark on each line citing a never-fetched source; undefined while still being revised. */
export function deliveredReport(content: string, v: Verdict | null | undefined, fetched: string[]): string | undefined {
  if (!v || !(v.passed || v.limitReached)) return undefined;
  if (v.passed) return content;
  const known = new Set(fetched);
  const lines = content.split('\n').map((l) => {
    const bad = citedIds(l).filter((id) => !known.has(id));
    return bad.length ? `${l} (not checked: ${bad.join(', ')} never fetched)` : l;
  });
  const failed = feedback(v.checks).map((l) => `> - ${l}`).join('\n');
  return `> **Not fully checked.** The revision limit was reached with these checks failing:\n${failed}\n\n${lines.join('\n')}`;
}

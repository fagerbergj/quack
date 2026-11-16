// The github.review grammar (contracts:038d6f48-11fe-48e3-8953-5a90e61b3f2c v6). The web is a core package and cannot import
// @quack/github, so this mirrors packages/github/src/review.ts; test-fixtures/review/ holds both to the same readings.
export type Finding = { index: number; label: string; anchor?: string; path?: string; line?: number; text: string };
export type Reading = { sections: Record<string, string>; verdict?: string; findings: Finding[]; unparsed: string[] };
/** A finding plus what the card shows: a one-sentence title and the rest, "why and fix", for a disclosure. */
export type Item = Finding & { title: string; more: string };
export type Review = Reading & { takeaway: string; items: Item[]; verified: string[]; notes: string[] };

const FINDING = /^- (\w+)(?:\s*\((\w+)\))?:\s*(.*)$/;
const CITATION = /^`?((?:\.{1,2}\/)?[\w.@+-]*\/[\w.@/+-]+|[\w@+-][\w@+-]*\.[\w.@+-]+|[A-Z][\w-]*):(\d+)(?:-\d+)?`?\s*[—–-]?\s*(.*)$/s;
const FENCE = /^(```|~~~)/;
const BULLET = /^(- |[*+] |\d+\. )/;

// Whether each line sits outside a code fence; a fence's own lines count as inside.
function outsideFences(lines: string[]): boolean[] {
  let open = false;
  return lines.map((l) => {
    const edge = FENCE.test(l);
    const inside = open || edge;
    if (edge) open = !open;
    return !inside;
  });
}

function findingsOf(section: string): Pick<Reading, 'findings' | 'unparsed'> {
  const lines = section.split('\n');
  const free = outsideFences(lines);
  const out: Pick<Reading, 'findings' | 'unparsed'> = { findings: [], unparsed: [] };
  lines.forEach((line, i) => {
    const m = free[i] ? FINDING.exec(line) : null;
    const last = out.findings.at(-1);
    if (m) out.findings.push(toFinding(m, out.findings.length + 1));
    else if (free[i] && BULLET.test(line)) out.unparsed.push(line);
    else if (last && line.trim()) last.text += `\n${line}`;
  });
  return out;
}

function toFinding(m: RegExpExecArray, index: number): Finding {
  const c = CITATION.exec(m[3]);
  return { index, label: m[1], anchor: m[2], path: c?.[1], line: c ? Number(c[2]) : undefined, text: c ? c[3] : m[3] };
}

/** The review Markdown as the contract reads it; the same reading the github package's parser gives. */
export function readReview(md: string): Reading {
  const sections: Record<string, string> = {};
  let key: string | undefined;
  for (const line of md.split('\n')) {
    const heading = /^## (\w+)/.exec(line);
    key = heading ? heading[1].toLowerCase() : key;
    if (key && !heading) sections[key] = `${sections[key] ?? ''}${line}\n`;
  }
  return { sections, verdict: sections.verdict?.trim().split(/\s/)[0]?.toLowerCase(), ...findingsOf(sections.findings ?? '') };
}

// Top-level "- " lines start an item; a "- " inside a code fence belongs to the item above.
function items(text: string): string[] {
  const out: string[] = [];
  let fenced = false;
  for (const line of text.split('\n')) {
    if (line.startsWith('```')) fenced = !fenced;
    if (!fenced && line.startsWith('- ')) out.push(line.slice(2));
    else if (out.length) out[out.length - 1] += `\n${line}`;
  }
  return out.map((i) => i.trim());
}

const SENTENCE = /^([\s\S]{16,}?(?<!\b(?:e\.g|i\.e))[.!?])\s+(?=[A-Z`])/;
const TITLE_MAX = 140;
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function cutTitle(s: string): string {
  const t = s.length > TITLE_MAX ? s.slice(0, TITLE_MAX).replace(/\s+\S*$/, '') : s;
  return t.length < s.length ? `${t}…` : t;
}

function item(f: Finding): Item {
  const [first, ...tail] = f.text.split('\n');
  const head = SENTENCE.exec(first);
  const title = cap(cutTitle(head ? head[1] : first));
  // The first paragraph's remainder and everything after it (a code block, say) is the "why and fix".
  const lead = title.endsWith('…') ? first : head ? first.slice(head[0].length) : '';
  return { ...f, title, more: [lead, ...tail].join('\n').trim() };
}

/** A review for the card; its `verdict` is undefined (or empty) when the text is not one. */
export function parseReview(md: string): Review {
  const r = readReview(md);
  return { ...r, takeaway: (r.sections.takeaway ?? '').trim(), items: r.findings.map(item), verified: items(r.sections.verified ?? ''), notes: items(r.sections.notes ?? '') };
}

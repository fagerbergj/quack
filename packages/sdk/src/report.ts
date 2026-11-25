// The research report's Markdown rules: its sections, the source ids it cites, and its findings.

const CITATION = /\[(s\d+)\]/g;
const FENCE = /^\s*(`{3,}|~{3,})/;

/** Each line with whether it sits in a code fence; a fence's own lines count as fenced. */
export function fenced(text: string): Array<[string, boolean]> {
  let open = '';
  return text.split('\n').map((line) => {
    const mark = FENCE.exec(line)?.[1][0];
    const inside = !!open || !!mark;
    if (mark && (!open || mark === open)) open = open ? '' : mark;
    return [line, inside];
  });
}

export type Section = { name: string; heading: string; lines: string[] };

/** The one heading rule every reader uses: a section starts at a line "## name" outside code fences and is keyed by its name, trimmed and
 *  lowercased; the preamble before the first heading belongs to no section. */
export function splitReport(text: string): { preamble: string[]; sections: Section[] } {
  const out = { preamble: [] as string[], sections: [] as Section[] };
  for (const [line, code] of fenced(text)) {
    if (!code && line.startsWith('## ')) out.sections.push({ name: line.slice(3).trim().toLowerCase(), heading: line, lines: [] });
    else (out.sections.at(-1)?.lines ?? out.preamble).push(line);
  }
  return out;
}

/** The trimmed bodies of the named sections, joined by a newline. */
export const sectionBody = (text: string, names: string[]) => splitReport(text).sections.filter((s) => names.includes(s.name)).map((s) => s.lines.join('\n').trim()).join('\n');

/** The report's own text: code fences and inline code are quoted material, so a mark there cites nothing. */
const ownText = (text: string) => fenced(text).filter(([, code]) => !code).map(([line]) => line.replace(/(`+)[^\n]*?\1/g, ' ')).join('\n');

/** The source ids a text cites, each once, in order of first citation. */
export const citedIds = (text: string) => [...new Set([...ownText(text).matchAll(CITATION)].map((m) => m[1]))];

const ITEM = /^(\s*)([-*]|\d+\.)\s/;
type Item = { indent: number; lines: string[]; context: string[]; kids: number };
type Parse = { items: Item[]; open: Item[]; para: string[]; intro: string[]; gap: boolean; fence: boolean };
// A bullet ending in a colon (marks and emphasis aside) that has sub-bullets only introduces them: it is their context, not a finding.
const leads = (i: Item) => i.kids > 0 && /:$/.test(i.lines.join(' ').replace(/\s*\[s\d+\]/g, '').replace(/[\s*_`]+$/, ''));

function bullet(p: Parse, line: string, indent: number) {
  p.open = p.open.filter((o) => o.indent < indent);
  const parent = p.open.at(-1);
  // A list's lead-in is the paragraph directly above it; a code block or table between them does not part them.
  if (!parent && p.para.length) [p.intro, p.para] = [p.para, []];
  if (parent) parent.kids++;
  // Context is there to cite the bullet; an unmarked lead line would only reframe the claim for the judge.
  const marked = (lines: string[]) => (/\[s\d+\]/.test(lines.join(' ')) ? lines : []);
  const item = { indent, lines: [line], context: parent ? [...parent.context, ...marked(parent.lines)] : marked(p.intro), kids: 0 };
  p.items.push(item);
  p.open.push(item);
}

function readLine(p: Parse, line: string) {
  const m = ITEM.exec(line);
  if (/^```/.test(line)) [p.fence, p.open] = [!p.fence, []];
  else if (p.fence || /^\|/.test(line)) p.open = [];
  else if (m) bullet(p, line, m[1].length);
  else if (/^#/.test(line)) [p.open, p.para, p.intro] = [[], [], []];
  else if (line.trim() && p.open.length && (!p.gap || /^\s{2}/.test(line))) p.open.at(-1)!.lines.push(line);
  else if (line.trim()) [p.open, p.para] = [[], p.gap ? [line] : [...p.para, line]];
  p.gap = !line.trim();
}

/** Every bullet under the "## Findings" heading, with its first line (which names it in feedback) and as text its own lines after its context:
 *  the bullets it sits in and the paragraph above its list, whose marks cite it too. */
export function findingItems(report: string): Array<{ first: string; text: string }> {
  const p: Parse = { items: [], open: [], para: [], intro: [], gap: false, fence: false };
  for (const s of splitReport(report).sections.filter((x) => x.name === 'findings')) for (const line of [...s.lines, '#']) readLine(p, line);
  return p.items.filter((i) => !leads(i)).map((i) => ({ first: i.lines[0].trim(), text: [...i.context, ...i.lines].join('\n') }));
}

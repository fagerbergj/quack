import { BLUE, cut, plain, type Extension } from '../extension.ts';
import { Blurb } from '../Markdown.tsx';

// The question up to its first ?, , or . within 40 characters, else a word-boundary cut.
function question(s: string): string {
  const i = s.slice(0, 40).search(/[?,.]/);
  return i < 0 ? cut(s) : s.slice(0, i + (s[i] === '?' ? 1 : 0)).trim();
}

const BULLET = /^(\s*)([-*]|\d+\.)\s/;
const MARK = /\s*\[s\d+\]/g;
type Bullet = { indent: number; lines: string[]; context: string[]; kids: number };
type Read = { all: Bullet[]; open: Bullet[]; on: boolean; fence: boolean; gap: boolean };

function readLine(r: Read, line: string) {
  const m = BULLET.exec(line);
  if (/^\s*(`{3,}|~{3,})/.test(line)) [r.fence, r.open] = [!r.fence, []];
  else if (r.fence) return;
  else if (line.startsWith('## ')) [r.on, r.open] = [line.slice(3).trim().toLowerCase() === 'findings', []];
  else if (r.on && m) {
    r.open = r.open.filter((o) => o.indent < m[1]!.length);
    const parent = r.open.at(-1);
    if (parent) parent.kids++;
    const item = { indent: m[1]!.length, lines: [line], context: parent ? [...parent.context, ...(/\[s\d+\]/.test(parent.lines.join(' ')) ? parent.lines : [])] : [], kids: 0 };
    r.all.push(item);
    r.open.push(item);
  } else if (line.trim() && r.open.length && (!r.gap || /^\s{2}/.test(line))) r.open.at(-1)!.lines.push(line);
  else if (line.trim()) r.open = [];
  r.gap = !line.trim();
}

const SENTENCE = /(?<=[.!?])\s+(?=["“(*_]?[\p{Lu}\p{N}])/u;
const leadIn = (t: string) => /:[\s*_]*(\[s\d+\]\s*)*$/.test(t);

/** The Nth finding's Kth sentence, numbered as the research judge names its items ("finding-3.2"), marks stripped.
 *  shortcut: reads bullet lists only, without the cited paragraph above a list that the judge also counts; upgrade if verdicts carry the claim. */
export function claimOf(report: string, n: number, k: number): string | undefined {
  const r: Read = { all: [], open: [], on: false, fence: false, gap: false };
  for (const line of report.split('\n')) readLine(r, line);
  const f = r.all.filter((b) => !(b.kids > 0 && leadIn(b.lines.join(' ').replace(/[\s*_`]+$/, ''))))[n - 1];
  const sentences = f && [...f.context, ...f.lines].map((l) => l.replace(BULLET, '').trim()).flatMap((l) => l.split(SENTENCE))
    .filter((t) => /[\p{L}\p{N}]/u.test(t.replace(MARK, '')) && !leadIn(t));
  return sentences?.[k - 1]?.replace(MARK, '').trim();
}

// A grouped note names its claim, then the item by scope: "finding-3.2: source finding-3.2 ~ s4: why".
const ITEM = /^(?:finding-\d+\.\d+: )?(?:source )?finding-(\d+)\.(\d+) ~ (s\d+): ([\s\S]*)$/;
const FLAGGED = /^flagged by (\S+) \(no reason given\)/;

/** A failing claim item: which claim and source, the claim's words, and why; a reasonless flag reads short, its full note in the tooltip. */
function claimNote(report: string, note: string) {
  const m = ITEM.exec(note);
  const claim = m && claimOf(report, Number(m[1]), Number(m[2]));
  if (!m || !claim) return undefined;
  const flagged = FLAGGED.exec(m[4]!);
  return (
    <div className="flex flex-col items-start gap-0.5 border-l-2 border-danger-line py-1 pl-2.5">
      <span className="rounded-md bg-soft px-1.5 py-0.5 text-[11px] font-medium text-ink-2">{`finding-${m[1]}.${m[2]} · ${m[3]}`}</span>
      <span title={claim} className="line-clamp-2 text-ink">{`“${claim}”`}</span>
      <span className="text-[12px] text-ink-2">{`→ ${m[3]}`}</span>
      {flagged ? <span title={m[4]} className="text-[12.5px] text-muted italic">{`Flagged by ${flagged[1] === 'clef' ? 'Clef' : flagged[1]}, no reason given`}</span> : <Blurb text={m[4]!} className="text-[12.5px]" />}
    </div>
  );
}

export const research: Extension = {
  artifact: (a) => (a.kind === 'research.report' ? { name: 'Report' } : undefined),
  note: (kind, content, note) => (kind === 'research.report' ? claimNote(content, note) : undefined),
  session: {
    label: 'research', match: (r) => r.agent === 'researcher' || r.key.startsWith('research:'), icon: 'M18 11a7 7 0 1 0-14 0a7 7 0 1 0 14 0M20 20l-3.5-3.5', tint: BLUE,
    name: (r) => plain(r.title ?? r.key, question(r.title ?? r.key)),
    words: () => ({ run: 'Researching', done: 'Report ready', none: 'No report yet' }),
  },
};

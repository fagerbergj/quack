import { claimOf } from '@quack/sdk/report';
import { BLUE, cut, plain, type Extension } from '../extension.ts';
import { Blurb } from '../Markdown.tsx';

// The question up to its first ?, , or . within 40 characters, else a word-boundary cut.
function question(s: string): string {
  const i = s.slice(0, 40).search(/[?,.]/);
  return i < 0 ? cut(s) : s.slice(0, i + (s[i] === '?' ? 1 : 0)).trim();
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

import type { Artifact } from '../api.ts';
import type { ListRow } from '../extension.ts';
import { StatusMark, type Mark } from '../StatusIcon.tsx';
import { prOf } from './pulls.ts';

// The seal's summary reads "<decision> · <tally>": "Approve · 2 suggestions, 3 nits", "Comment · no findings".
const DECISION: Record<string, [Mark, string]> = { Approve: ['ok', 'Approved'], 'Request changes': ['bad', 'Changes requested'] };

// The spaces between parts collapse in the flex row but keep the link's accessible name in words.
function Findings({ pr, tally }: { pr: string; tally: string }) {
  const counts = tally && tally !== 'no findings' ? tally.split(', ') : [];
  return (
    <>
      <span className="font-mono text-xs text-ink-2">{pr}</span>
      {tally === 'no findings' && ' No findings'}
      {counts.length > 0 && <>{' '}<span className="ml-1 hidden gap-1 sm:inline-flex">{counts.flatMap((c, i) => [...(i ? [' '] : []), <span key={c} className="rounded-md bg-soft px-1.5 text-[11px] text-ink-2 tabular-nums">{c}</span>])}</span></>}
    </>
  );
}

/** A review's Artifacts row: its PR ref and finding counts under the title, its decision as a mark, folded with the day's other reviews. */
export function reviewRow(latest: Artifact[]): ListRow | undefined {
  const a = latest.find((x) => x.kind === 'github.review');
  const pr = a && prOf(a.session);
  if (!pr) return undefined;
  const [decision = '', tally = ''] = (a.summary ?? '').split(' · ');
  const mark = DECISION[decision];
  return {
    line: <Findings pr={`${pr.name}#${pr.number}`} tally={tally} />,
    end: mark && <StatusMark mark={mark[0]} label={mark[1]} />,
    fold: { label: 'pull request reviews', group: pr.name, ok: decision === 'Approve' },
  };
}

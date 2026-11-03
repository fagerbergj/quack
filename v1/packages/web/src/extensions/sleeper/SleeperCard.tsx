import { useContext } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { SessionKeyContext } from '../../extension.ts';
import { Full, titleOf } from './cards.tsx';
import { Verdict } from './parts.tsx';
import { LineupCard, type Card } from './Sleeper.tsx';
import type { Result } from './week.ts';

const KINDS = ['lineup', 'waivers', 'trade-finder', 'trade', 'trends', 'digest', 'retro', 'history'];

/** The card a sealed sleeper.cards artifact holds, or undefined when it is not JSON of a known kind. */
export function cardOf(a: SealedArtifact): Record<string, any> | undefined {
  if (a.kind !== 'sleeper.cards') return undefined;
  try {
    const c = JSON.parse(a.content) as Record<string, any> | null;
    return c && KINDS.includes(c.kind) ? c : undefined;
  } catch {
    return undefined;
  }
}

// Shape mismatches throw while drawing; ArtifactBoundary then shows the JSON tree.
export function SleeperCard({ a, card }: { a: SealedArtifact; card: Record<string, any> }) {
  const key = useContext(SessionKeyContext) ?? '';
  if (card.kind === 'lineup') return <LineupCard card={card as Card} artifact={a} sessionKey={key} />;
  const r: Result = { key, week: card.week, job: card.kind, running: false, artifact: a, card };
  const title = titleOf(r, true);
  return (
    <article aria-label={title} className="overflow-hidden rounded-[22px] border border-line bg-surface shadow-card">
      <div className="flex flex-col gap-2 border-b border-line px-4 py-3">
        <h2 className="m-0 text-sm font-semibold">{title}</h2>
        <Verdict artifact={a} />
      </div>
      <div className="min-w-0 px-4 pb-3 [overflow-wrap:anywhere]"><Full r={r} /></div>
    </article>
  );
}

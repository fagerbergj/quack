import type { SealedArtifact } from '../../api.ts';
import { plain, sentence, type Extension } from '../../extension.ts';
import { Sleeper } from './Sleeper.tsx';

const KEY = /^sleeper:[^:]+:(\d+):(lineup|waivers)$/;

const NAMES: Record<string, string> = { lineup: 'Lineup picks', waivers: 'Waiver claims', 'trade-finder': 'Trade ideas', trade: 'Trade check', trends: 'Trends', history: 'League history' };

// Each card kind names its job; weekly kinds name their week.
function cardName(a: SealedArtifact): string {
  try {
    const c = JSON.parse(a.content) as { kind?: string; week?: unknown; past?: unknown };
    if (c.kind && NAMES[c.kind]) return NAMES[c.kind]!;
    if (c.kind === 'digest' && typeof c.week === 'number') return `Week ${c.week} ${c.past ? 'recap' : 'preview'}`;
    if (c.kind === 'retro' && typeof c.week === 'number') return `Week ${c.week} retro`;
  } catch { /* an unparsable card still reads as a card */ }
  return '';
}

export const sleeper: Extension = {
  artifact: (a) => (a.kind === 'sleeper.cards' ? { name: cardName(a) } : undefined),
  session: {
    label: 'sleeper', match: (r) => KEY.test(r.key), icon: 'M8 4h8v5a4 4 0 0 1-8 0ZM8 6H5a3 3 0 0 0 3 4M16 6h3a3 3 0 0 1-3 4M12 13v4M9 20h6', tint: 'bg-success-bg text-success',
    name: (r) => {
      const [, week, job] = KEY.exec(r.key)!;
      return plain(`${sentence(job!)} · wk ${week}`);
    },
    words: () => ({ kind: 'sleeper.cards', w: { run: 'Checking lineup', pass: 'Lineup picks ready', fail: "Didn't pass its checks", none: 'Nothing yet' } }),
  },
  nav: { route: 'sleeper', label: 'Sleeper', icon: 'M8 4h8v5a4 4 0 0 1-8 0zM12 13v4M8 21h8M16 6h3v2a3 3 0 0 1-3 3M8 6H5v2a3 3 0 0 0 3 3', page: Sleeper },
};

import type { SealedArtifact } from '../../api.ts';
import { plain, sentence, type Extension } from '../../extension.ts';
import { createElement } from 'react';
import { Sleeper } from './Sleeper.tsx';
import { SleeperCard, cardOf } from './SleeperCard.tsx';

const KEY = /^sleeper:[^:]+:(\d+):(lineup|waivers|history|trends|trade-finder|trade|digest|retro)(?::|$)/;

type Job = { noun: string; title: (week: string) => string; run: string; pass: (week: string) => string };
const JOB: Record<string, Job> = {
  lineup: { noun: 'Lineup', title: (w) => `Lineup · wk ${w}`, run: 'Checking your lineup', pass: () => 'Lineup picks ready' },
  waivers: { noun: 'Waivers', title: (w) => `Waivers · wk ${w}`, run: 'Looking at waivers', pass: () => 'Waiver claims ready' },
  history: { noun: 'History', title: () => 'History', run: 'Reading league history', pass: () => 'League history ready' },
  trends: { noun: 'Trends', title: (w) => `Trends · wk ${w}`, run: 'Reading trends', pass: () => 'Trends ready' },
  'trade-finder': { noun: 'Trade finder', title: (w) => `Trade finder · wk ${w}`, run: 'Looking for trades', pass: () => 'Trade ideas ready' },
  // The key carries only a roster number, so the title leaves the partner out.
  trade: { noun: 'Trade', title: (w) => `Trade · wk ${w}`, run: 'Looking for trades', pass: () => 'Trade ideas ready' },
  digest: { noun: 'Recap', title: (w) => `Recap · wk ${w}`, run: 'Writing the recap', pass: (w) => `Week ${w} recap ready` },
  retro: { noun: 'Retro', title: (w) => `Retro · wk ${w}`, run: 'Writing the retro', pass: (w) => `Week ${w} retro ready` },
};

const jobOf = (key: string) => { const [, week, job] = KEY.exec(key)!; return { week: week!, job: JOB[job!]! }; };

const NAMES: Record<string, string> = { lineup: 'Lineup picks', waivers: 'Waiver claims', 'trade-finder': 'Trade ideas', trade: 'Trade check', trends: 'Trends', history: 'League history' };

// Each card kind names its job; weekly kinds name their week.
function cardName(a: SealedArtifact): string {
  try {
    const c = JSON.parse(a.content) as { kind?: string; week?: unknown; past?: unknown };
    if (c.kind && NAMES[c.kind]) return NAMES[c.kind]!;
    if (c.kind === 'digest' && typeof c.week === 'number') return `Week ${c.week} ${c.past ? 'recap' : 'preview'}`;
    if (c.kind === 'retro' && typeof c.week === 'number') return `Week ${c.week} retro`;
  } catch { /* an unparsable card falls through to the generic Ready line */ }
  return '';
}

// Card and page draw the same view; undefined (not a known card) leaves the generic JSON tree.
const view = (a: SealedArtifact) => {
  const card = cardOf(a);
  return card && createElement(SleeperCard, { key: a.hash, a, card });
};

export const sleeper: Extension = {
  card: view,
  full: view,
  wholeUnits: ['card', 'facts'],
  artifact: (a) => (a.kind === 'sleeper.cards' ? { name: cardName(a), titled: true } : undefined),
  session: {
    label: 'sleeper', noun: (r) => jobOf(r.key).job.noun, match: (r) => KEY.test(r.key), icon: 'M8 4h8v5a4 4 0 0 1-8 0ZM8 6H5a3 3 0 0 0 3 4M16 6h3a3 3 0 0 1-3 4M12 13v4M9 20h6', tint: 'bg-success-bg text-success',
    name: (r) => {
      const { week, job } = jobOf(r.key);
      return plain(job.title(week));
    },
    words: (r) => {
      const { week, job } = jobOf(r.key);
      return { run: job.run, done: job.pass(week), none: 'Nothing yet' };
    },
  },
  nav: { route: 'sleeper', label: 'Sleeper', icon: 'M8 4h8v5a4 4 0 0 1-8 0zM12 13v4M8 21h8M16 6h3v2a3 3 0 0 1-3 3M8 6H5v2a3 3 0 0 0 3 3', page: Sleeper, pinned: true },
};

import { plain, type Extension } from '../../extension.ts';
import { createElement } from 'react';
import { JOBS, jobTitle, parseKey } from './cards.tsx';
import { Sleeper } from './Sleeper.tsx';
import { SleeperCard, cardOf } from './SleeperCard.tsx';

// Only a key that parsed reaches these.
const jobOf = (key: string) => parseKey(key)!;

export const sleeper: Extension = {
  // Undefined (not a known card) leaves the generic JSON tree.
  card: (a) => {
    const card = cardOf(a);
    return card && createElement(SleeperCard, { key: a.hash, a, card });
  },
  wholeUnits: ['card', 'facts'],
  artifact: (a) => {
    if (a.kind !== 'sleeper.cards') return undefined;
    const c = cardOf(a);
    // An unreadable card falls through to the generic Ready line.
    return { name: c && typeof c.week === 'number' ? jobTitle(c.kind, c.week, c) : '' };
  },
  session: {
    label: 'sleeper', noun: (r) => JOBS[jobOf(r.key).job]!.label, match: (r) => parseKey(r.key) !== undefined, icon: 'M8 4h8v5a4 4 0 0 1-8 0ZM8 6H5a3 3 0 0 0 3 4M16 6h3a3 3 0 0 1-3 4M12 13v4M9 20h6', tint: 'bg-success-bg text-success',
    name: (r) => {
      const { week, job } = jobOf(r.key);
      return plain(jobTitle(job, week));
    },
    words: (r) => {
      const { week, job } = jobOf(r.key);
      return { run: JOBS[job]!.run, done: `${jobTitle(job, week)} ready`, none: 'Nothing yet' };
    },
  },
  nav: { route: 'sleeper', label: 'Sleeper', icon: 'M8 4h8v5a4 4 0 0 1-8 0zM12 13v4M8 21h8M16 6h3v2a3 3 0 0 1-3 3M8 6H5v2a3 3 0 0 0 3 3', page: Sleeper, pinned: true },
};

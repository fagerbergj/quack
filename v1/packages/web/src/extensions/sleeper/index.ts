import { plain, sentence, type Extension } from '../../extension.ts';
import { Sleeper } from './Sleeper.tsx';

const KEY = /^sleeper:[^:]+:(\d+):(lineup|waivers)$/;

export const sleeper: Extension = {
  session: {
    label: 'sleeper', match: (r) => KEY.test(r.key), icon: 'M8 4h8v5a4 4 0 0 1-8 0ZM8 6H5a3 3 0 0 0 3 4M16 6h3a3 3 0 0 1-3 4M12 13v4M9 20h6', tint: 'bg-success-bg text-success',
    name: (r) => {
      const [, week, job] = KEY.exec(r.key)!;
      return plain(`${sentence(job!)} · wk ${week}`);
    },
    words: () => ({ kind: 'sleeper.cards', w: { run: 'Checking lineup', pass: 'Card ready', fail: 'Checks failed', none: 'No card yet' } }),
  },
  nav: { route: 'sleeper', label: 'Sleeper', icon: 'M8 4h8v5a4 4 0 0 1-8 0zM12 13v4M8 21h8M16 6h3v2a3 3 0 0 1-3 3M8 6H5v2a3 3 0 0 0 3 3', page: Sleeper },
};

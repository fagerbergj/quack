import { useEffect, useState } from 'react';
import { BLUE, plain, type Extension } from '../../extension.ts';
import { documentTree, notebooks } from './api.ts';
import { Remarkable } from './Remarkable.tsx';

// No line while loading, or when the service is not registered; the tile still opens the page, which says why.
function useStatus() {
  const [changed, setChanged] = useState<number>();
  useEffect(() => {
    let live = true;
    documentTree().then((t) => live && setChanged(notebooks(t).filter((n) => n.state === 'changed').length), () => {});
    return () => void (live = false);
  }, []);
  return changed === undefined ? undefined : { text: changed ? `${changed} changed` : 'Up to date', warn: changed > 0 };
}

const ICON = 'M4 20h4L19 9l-4-4L4 16ZM13.5 6.5l4 4';

export const remarkable: Extension = {
  session: {
    label: 'reMarkable', noun: () => 'Transcript', group: 'Notes', match: (r) => r.key.startsWith('remarkable:'), icon: ICON, tint: BLUE,
    name: (r) => plain(r.title ?? r.key.slice('remarkable:'.length)),
    words: () => ({ run: 'Transcribing', done: 'Transcribed', none: 'Not transcribed yet' }),
  },
  artifact: (a) => (a.kind === 'remarkable.transcript' ? { name: 'Transcript' } : undefined),
  nav: { route: 'remarkable', label: 'reMarkable', icon: ICON, tint: 'bg-paper text-paper-ink', page: Remarkable, status: useStatus },
};

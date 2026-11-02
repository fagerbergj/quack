import { BLUE, plain, type Extension } from '../extension.ts';

export const remarkable: Extension = {
  session: {
    label: 'reMarkable', match: (r) => r.key.startsWith('remarkable:'), icon: 'M4 20h4L19 9l-4-4L4 16ZM13.5 6.5l4 4', tint: BLUE,
    name: (r) => plain(r.title ?? r.key.slice('remarkable:'.length)),
    words: () => ({ kind: 'remarkable.transcript', w: { run: 'Transcribing', pass: 'Transcribed ✓', fail: 'Failed: checks did not pass', none: 'Not transcribed yet', sealed: 'Transcribed · not checked' } }),
  },
  nav: { route: 'remarkable', label: 'reMarkable', icon: 'M4 20l4-1 11-11-3-3L5 16zM14 6l3 3' },
};

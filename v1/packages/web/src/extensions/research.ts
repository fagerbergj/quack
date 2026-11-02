import { BLUE, cut, plain, type Extension } from '../extension.ts';

// The question up to its first ?, , or . within 40 characters, else a word-boundary cut.
function question(s: string): string {
  const i = s.slice(0, 40).search(/[?,.]/);
  return i < 0 ? cut(s) : s.slice(0, i + (s[i] === '?' ? 1 : 0)).trim();
}

export const research: Extension = {
  suggestion: { label: 'Research a question', prompt: 'Research: ' },
  artifact: (a) => (a.kind === 'research.report' ? { name: 'Report' } : undefined),
  session: {
    label: 'research', match: (r) => r.agent === 'researcher' || r.key.startsWith('research:'), icon: 'M18 11a7 7 0 1 0-14 0a7 7 0 1 0 14 0M20 20l-3.5-3.5', tint: BLUE,
    name: (r) => plain(r.title ?? r.key, question(r.title ?? r.key)),
    words: () => ({ kind: 'research.report', w: { run: 'Researching', pass: 'Report ✓', fail: 'Report ✗', none: 'No report yet' } }),
  },
};

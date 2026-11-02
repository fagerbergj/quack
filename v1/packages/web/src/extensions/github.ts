import { BLUE, cut, sentence, type Extension } from '../extension.ts';
import type { Row } from '../sessions.ts';

const KEY = /^github:([^/]+)\/([^#]+)#(\d+)$/;
// Sessions opened before titles were recorded carry the trigger prompt as their title; the PR's own title follows "Title: ".
const TRIGGER = /^Review pull request .*? Title: (.+)$/s;
const CC_PREFIX = /^\w+(\([^)]*\))?!?:\s*/;

function prTitle(r: Row, fallback: string): string {
  const t = (r.title && TRIGGER.exec(r.title)?.[1]) || (r.title?.startsWith('Review pull request ') ? '' : r.title) || '';
  return t.replace(CC_PREFIX, '') ? sentence(t.replace(CC_PREFIX, '')) : fallback;
}

export const github: Extension = {
  session: {
    label: 'review', match: (r) => KEY.test(r.key), icon: 'M3.5 6a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M15.5 18a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M6 8.5v11M18 15.5V9a3 3 0 0 0-3-3h-4M13 3.5 10.5 6 13 8.5', tint: BLUE,
    name: (r) => {
      const [, owner, repo, n] = KEY.exec(r.key)!;
      const full = prTitle(r, repo!);
      return { title: cut(full), full, num: `#${n}`, ...(r.multiRepo && { repo: `${owner}/${repo}` }) };
    },
    words: () => ({ kind: 'github.review', w: { run: 'Reviewing', pass: 'Approved ✓', fail: "Didn't pass its checks", none: 'Not reviewed yet' } }),
  },
  start: { prefix: 'github', label: 'GitHub' },
  // GitHub redirects /pull/<n> to /issues/<n> for a plain issue, so one URL fits both.
  link: (key) => {
    const m = KEY.exec(key);
    return m ? { url: `https://github.com/${m[1]}/${m[2]}/pull/${m[3]}`, label: 'View on GitHub' } : undefined;
  },
  scope: (key) => /^github:([^#]+)#/.exec(key)?.[1],
};

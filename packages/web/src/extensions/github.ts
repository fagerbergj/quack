import { createElement } from 'react';
import { BLUE, cut, sentence, type Extension, type Nameable } from '../extension.ts';
import type { SealedArtifact } from '../api.ts';
import { parseOpening, PrCard, type PrMeta } from './PrCard.tsx';
import { PrSection } from './PrRail.tsx';
import { PrChip, PrStates, PrStatesNotice } from './prState.tsx';
import { parseReview } from './review.ts';
import { reviewCard } from './ReviewCard.tsx';

const KEY = /^github:([^/]+)\/([^#]+)#(\d+)$/;
// Sessions opened before titles were recorded carry the trigger prompt as their title; the PR's own title follows "Title: ".
const TRIGGER = /^Review pull request .*? Title: (.+)$/s;
const CC_PREFIX = /^\w+(\([^)]*\))?!?:\s*/;

function prTitle(r: Nameable, fallback: string): string {
  const t = (r.title && TRIGGER.exec(r.title)?.[1]) || (r.title?.startsWith('Review pull request ') ? '' : r.title) || '';
  return t.replace(CC_PREFIX, '') ? sentence(t.replace(CC_PREFIX, '')) : fallback;
}

const KIND = 'github.review';

// A draft is outdated while a run works on a newer head; the newest invocation's meta records that head.
function outdated(a: SealedArtifact, now: Parameters<NonNullable<Extension['card']>>[1]): boolean {
  const head = (now?.meta.at(-1) as PrMeta | undefined)?.headSha;
  return Boolean(now?.running && a.basis && head && head !== a.basis);
}
const DECISION: Record<string, string> = { approve: 'Approve', request_changes: 'Request changes', comment: 'Comment' };

const postedTo = (a: SealedArtifact) => {
  const decision = DECISION[parseReview(a.content)?.verdict ?? ''];
  return `Posted to GitHub${decision ? ` as ${decision}` : ''}`;
};

export const github: Extension = {
  wholeUnits: ['review'],
  session: {
    label: 'review', noun: () => 'Review', match: (r) => KEY.test(r.key), icon: 'M3.5 6a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M15.5 18a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M6 8.5v11M18 15.5V9a3 3 0 0 0-3-3h-4M13 3.5 10.5 6 13 8.5', tint: BLUE,
    name: (r) => {
      const [, owner, repo, n] = KEY.exec(r.key)!;
      const full = prTitle(r, repo!);
      return { title: cut(full), full, num: `#${n}`, ...(r.multiRepo && { repo: `${owner}/${repo}` }) };
    },
    words: () => ({ run: 'Reviewing', done: 'Reviewed', none: 'Not reviewed yet' }),
  },
  artifact: (a, running) => (a.kind === KIND ? { name: 'Review', titled: true, posted: a.links?.length ? (running ? 'Previous review' : 'Review posted') : undefined, previous: running && Boolean(a.links?.length), postedTo: postedTo(a) } : undefined),
  card: (a, now) => (a.kind === KIND ? reviewCard(a, outdated(a, now)) : undefined),
  steps: { checkout_pr: () => 'Checking out the latest commit', submit_review: () => 'Waiting for the double-check' },
  // The card's extras are keyed by head sha; a review started by a comment has no head in its text and takes the newest.
  opening: (text, meta) => {
    const o = parseOpening(text);
    const records = meta as readonly PrMeta[];
    return o && createElement(PrCard, { o, meta: o.head === undefined ? records.at(-1) : records.find((m) => m.id === o.head) });
  },
  start: { prefix: 'github', label: 'GitHub' },
  chip: PrChip,
  provider: PrStates,
  notice: PrStatesNotice,
  rail: PrSection,
  // GitHub redirects /pull/<n> to /issues/<n> for a plain issue, so one URL fits both.
  link: (key) => {
    const m = KEY.exec(key);
    return m ? { url: `https://github.com/${m[1]}/${m[2]}/pull/${m[3]}`, label: 'Open on GitHub' } : undefined;
  },
  scope: (key) => /^github:([^#]+)#/.exec(key)?.[1],
};

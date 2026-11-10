import { createElement } from 'react';
import { BLUE, cut, sentence, type Extension, type Nameable } from '../extension.ts';
import type { SealedArtifact } from '../api.ts';
import { headMeta, parseOpening, PrCard, type PrMeta } from './PrCard.tsx';
import { PrSection } from './PrRail.tsx';
import { PrChip, PrStates, PrStatesNotice, prUrl } from './prState.tsx';
import { prOf } from './pulls.ts';
import { parseReview } from './review.ts';
import { reviewCard } from './ReviewCard.tsx';

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
    label: 'review', noun: () => 'Review', match: (r) => prOf(r.key) !== undefined, icon: 'M3.5 6a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M15.5 18a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M6 8.5v11M18 15.5V9a3 3 0 0 0-3-3h-4M13 3.5 10.5 6 13 8.5', tint: BLUE,
    name: (r) => {
      const { repo, name, number } = prOf(r.key)!;
      const full = prTitle(r, name);
      return { title: cut(full), full, num: `#${number}`, ...(r.multiRepo && { repo }) };
    },
    words: () => ({ run: 'Reviewing', done: 'Reviewed', none: 'Not reviewed yet' }),
  },
  artifact: (a, running) => (a.kind === KIND ? { name: 'Review', posted: a.links?.length ? (running ? 'Previous review' : 'Review posted') : undefined, previous: running && Boolean(a.links?.length), postedTo: postedTo(a) } : undefined),
  card: (a, now) => (a.kind === KIND ? reviewCard(a, outdated(a, now)) : undefined),
  steps: { checkout_pr: () => 'Checking out the latest commit', submit_review: () => 'Waiting for the double-check' },
  opening: (text, meta) => {
    const o = parseOpening(text);
    return o && createElement(PrCard, { o, meta: headMeta(meta, o) });
  },
  start: { prefix: 'github', label: 'GitHub' },
  chip: PrChip,
  provider: PrStates,
  notice: PrStatesNotice,
  rail: PrSection,
  link: (key) => {
    const url = prUrl(key);
    return url ? { url, label: 'Open on GitHub' } : undefined;
  },
  scope: (key) => prOf(key)?.repo,
};

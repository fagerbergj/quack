import { render, screen, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test } from 'vitest';
import type { SealedArtifact } from '../api.ts';
import { github } from './github.ts';
import { parseOpening, PrCard, type PrMeta } from './PrCard.tsx';
import { reviewCard } from './ReviewCard.tsx';
import { parseReview } from './review.ts';

// Sealed prod reviews: PR #2002 (request_changes, then approve) and #2042's first draft (comment).
const md = (name: string) => readFileSync(join(process.cwd(), 'src/extensions/fixtures', `${name}.md`), 'utf8');
const sealed = (content: string, over: Partial<SealedArtifact> = {}): SealedArtifact => ({ kind: 'github.review', version: 1, hash: 'h', content, sealedAt: 1, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, ...over });

test('a review parses into its takeaway, findings, verified and notes', () => {
  const r = parseReview(md('review-changes'))!;
  expect(r.verdict).toBe('request_changes');
  expect(r.findings.map((f) => [f.severity, f.kind, f.loc])).toEqual([
    ['blocking', 'defect', 'v1/packages/index/src/publisher.ts:58'],
    ['suggestion', undefined, 'v1/packages/index/test/publisher.test.ts:105'],
  ]);
  expect(r.findings[0]!.title).toMatch(/^If `root.commit` for the direct-spend record rejects, .*…$/);
  expect(r.findings[0]!.more).toContain('```ts');
  expect(r.verified).toHaveLength(6);
  expect(r.notes).toHaveLength(3);
  expect(parseReview(md('review-comment'))!.findings.map((f) => f.kind)).toEqual([undefined, undefined, undefined, 'tests', undefined, undefined]);
  expect(parseReview('just some text')).toBeUndefined();
});

test('the review card shows the seal, counts, a posted link, findings with their severity and location', () => {
  const link = { label: 'Posted', url: 'https://github.com/o/r/pull/1#pullrequestreview-9' };
  render(<>{reviewCard(sealed(md('review-changes'), { links: [link], verdict: { passed: true, gatesFailed: [], score: 1, checks: [] } }))}</>);
  const card = screen.getByRole('region', { name: 'Review' });
  expect(within(card).getByText('Changes requested')).toBeInTheDocument();
  expect(card).toHaveTextContent('1 blocking · 1 suggestion');
  expect(within(card).getByRole('link', { name: 'Posted to GitHub ↗' })).toHaveAttribute('href', link.url);
  expect(within(card).getByText('Blocking · defect')).toBeInTheDocument();
  expect(within(card).getByText('v1/packages/index/src/publisher.ts:58')).toBeInTheDocument();
  expect(within(card).getAllByText('Why and a fix')).toHaveLength(2);
  expect(within(card).getByText('Verified · 6')).toBeInTheDocument();
  expect(within(card).getByText('Notes · 3')).toBeInTheDocument();
  expect(card).toHaveTextContent('Double-checked');
  // Verified, Notes and the full review start closed.
  expect(card.querySelectorAll('details[open]')).toHaveLength(0);
});

test('an approval with only a nit is approved, and a failed gate says so', () => {
  render(<>{reviewCard(sealed(md('review-approve'), { verdict: { passed: false, gatesFailed: [], score: 0.54, checks: [{ criterion: 'c', id: 'tests', label: 'Tests cover the change', weight: 1, met: 0, notes: [] }] } }))}</>);
  const card = screen.getByRole('region', { name: 'Review' });
  expect(within(card).getByText('Approved')).toBeInTheDocument();
  expect(card).toHaveTextContent('1 nit');
  expect(card).toHaveTextContent("Didn't pass its checks");
  expect(card).not.toHaveTextContent('0.54');
  expect(within(card).getByText('Tests cover the change')).toBeInTheDocument();
  expect(within(card).queryByRole('link', { name: /Posted/ })).toBeNull();
});

test('text that is not a review stays a plain line', () => {
  expect(reviewCard(sealed('draft one'))).toBeUndefined();
});

const OPENING = [
  'Review pull request fagerbergj/quack#2002 (label quack:review).',
  'Title: Cost per run',
  'Head: 9f43b50aaaa on feat/v1-cost-per-run; base: v1',
  '',
  '## What\n- `deployment.json` model entries take an optional `price`.',
].join('\n');

const META: PrMeta = {
  id: '9f43b50aaaa',
  files: [{ path: 'index/src/facts.ts', additions: 12, deletions: 6 }, { path: 'session/src/definition.ts', additions: 13, deletions: 5 }],
  checks: [{ name: 'check', status: 'completed', conclusion: 'success' }, { name: 'deploy', status: 'completed', conclusion: 'skipped' }],
  linked: [{ number: 2001, title: 'Cost per run' }],
};

test('the opening text parses, with or without a head line', () => {
  expect(parseOpening(OPENING)).toMatchObject({ repo: 'fagerbergj/quack', number: '2002', why: 'label quack:review', title: 'Cost per run', head: '9f43b50aaaa', headRef: 'feat/v1-cost-per-run', baseRef: 'v1' });
  expect(parseOpening('Review pull request o/r#3 (/review by ann).\nTitle: T\n\nBody')).toMatchObject({ why: '/review by ann', head: undefined, body: 'Body' });
  expect(parseOpening('Please look at this')).toBeUndefined();
});

test('the opening card shows the PR with collapsible description, changes, checks and linked issue', () => {
  render(<PrCard o={parseOpening(OPENING)!} meta={META} />);
  const card = screen.getByRole('region', { name: 'Pull request' });
  expect(within(card).getByRole('heading', { name: 'Cost per run' })).toBeInTheDocument();
  expect(card).toHaveTextContent('fagerbergj/quack · label quack:review');
  expect(within(card).getByRole('link', { name: 'GitHub ↗' })).toHaveAttribute('href', 'https://github.com/fagerbergj/quack/pull/2002');
  expect(within(card).getByText('feat/v1-cost-per-run')).toBeInTheDocument();
  expect(within(card).getByText('9f43b50')).toBeInTheDocument();
  expect(within(card).getByText('2 files')).toBeInTheDocument();
  expect(within(card).getByText('+25')).toBeInTheDocument();
  expect(within(card).getByText('−11')).toBeInTheDocument();
  expect(within(card).getByText('1 passing · 1 skipped')).toBeInTheDocument();
  expect(within(card).getByRole('link', { name: '#2001 Cost per run' })).toHaveAttribute('href', 'https://github.com/fagerbergj/quack/issues/2001');
  // Description and Changes start open, Checks and Linked issue closed; the description renders as markdown.
  expect(card.querySelectorAll('details[open]')).toHaveLength(2);
  expect(card.querySelector('code')).toHaveTextContent('deployment.json');
});

test('without meta the card still shows the header and description, and no empty sections', () => {
  render(<PrCard o={parseOpening(OPENING)!} />);
  const card = screen.getByRole('region', { name: 'Pull request' });
  expect(within(card).getByText('Description')).toBeInTheDocument();
  for (const name of ['Changes', 'Checks', 'Linked issue']) expect(within(card).queryByText(name)).toBeNull();
});

test('the opening card takes the record for its own head only; the newest record stands in only when the text names no head', () => {
  const rec = (id: string, path: string): PrMeta => ({ id, files: [{ path, additions: 1, deletions: 0 }] });
  const records = [rec('aaa1111', 'old/a.ts'), rec('bbb2222', 'new/b.ts')];
  const show = (text: string) => {
    const { unmount } = render(<>{github.opening!(text, records)}</>);
    const files = within(screen.getByRole('region', { name: 'Pull request' })).queryAllByText(/\.ts$/).map((e) => e.textContent);
    unmount();
    return files;
  };
  const text = (head: string) => `Review pull request o/r#3 (label quack:review).\nTitle: T\nHead: ${head} on f; base: v1\n\nBody`;
  expect(show(text('aaa1111'))).toEqual(['old/a.ts']);
  expect(show(text('bbb2222'))).toEqual(['new/b.ts']);
  expect(show(text('ccc3333'))).toEqual([]);
  expect(show('Review pull request o/r#3 (/review by ann).\nTitle: T\n\nBody')).toEqual(['new/b.ts']);
});

test('a draft written against an earlier head is marked and dimmed while a run reviews the latest', () => {
  const a = sealed(md('review-changes'), { basis: 'old' });
  const marker = 'About an earlier commit · reviewing the latest';
  const show = (head: string) => render(<>{github.card!(a, { running: true, meta: [{ id: 'x', headSha: 'old' }, { id: head, headSha: head }] })}</>);
  const view = show('new');
  expect(screen.getByText(marker)).toBeInTheDocument();
  expect(screen.getByText('Blocking · defect').closest('.opacity-60')).not.toBeNull();
  view.unmount();
  show('old');
  expect(screen.queryByText(marker)).toBeNull();
});

test('no marker once the run ends, or for a draft that never recorded its head', () => {
  const meta = [{ id: 'new', headSha: 'new' }];
  render(<>{github.card!(sealed(md('review-changes'), { basis: 'old' }), { running: false, meta })}{github.card!(sealed(md('review-changes')), { running: true, meta })}</>);
  expect(screen.queryByText(/About an earlier commit/)).toBeNull();
});

test('a carried approval says it is the same change as the earlier review, with no Double-checked', () => {
  const from = { sha: 'abcdef1234567890', url: 'https://github.com/o/r/pull/1#pullrequestreview-5' };
  render(<>{reviewCard(sealed(md('review-approve'), { verdict: null, carriedFrom: from }))}</>);
  const card = screen.getByRole('region', { name: 'Review' });
  expect(within(card).getByRole('link', { name: 'Same change as the approved review of abcdef12' })).toHaveAttribute('href', from.url);
  expect(card).not.toHaveTextContent('Double-checked');
});

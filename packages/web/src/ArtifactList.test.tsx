import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import type { Artifact, Session } from './api.ts';
import { ArtifactList, dayLabel } from './ArtifactList.tsx';
import { rowOf } from './sessions.ts';
import { sess } from './test-harness.ts';

// Local times, so the day each row lands on holds in any time zone.
const NOW = new Date(2026, 10, 22, 15, 0).getTime();
const at = (day: number, h: number, m = 0) => new Date(2026, 10, day, h, m).toISOString();
const art = (session: string, kind: string, version: number, sealedAt: string, summary?: string): Artifact => ({ session, kind, version, hash: `${session}:${kind}:${version}`, sealedAt, summary });
const review = (n: number, sealedAt: string, summary: string, repo = 'quack') => art(`github:fagerbergj/${repo}#${n}`, 'github.review', 1, sealedAt, summary);
const row = (key: string, over: object = {}) => rowOf(sess(key, over) as Session, false);

function serve(artifacts: Artifact[], contracts: object[] = []) {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => Response.json(url.startsWith('apps/contracts/') ? { contracts } : { artifacts })));
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const day = (name: string) => within(screen.getByRole('region', { name }));
// The list answers after the first render; the rows wait for it.
const listed = () => screen.findByRole('region', { name: 'Today' });

test('rows group under Today, Yesterday, then the weekday and date, in local time, each showing its clock time', async () => {
  expect(dayLabel(new Date(2026, 10, 22, 0, 1).getTime(), NOW)).toBe('Today');
  expect(dayLabel(new Date(2026, 10, 21, 23, 59).getTime(), NOW)).toBe('Yesterday');
  expect(dayLabel(new Date(2026, 10, 20, 12).getTime(), NOW)).toBe('Fri, Nov 20');
  serve([
    art('research:a', 'research.report', 1, at(22, 14, 51), 'Cold caches dominate'),
    art('research:b', 'research.report', 1, at(21, 0, 5), 'Seventy signals'),
    art('research:c', 'research.report', 1, at(20, 9), 'Older'),
  ]);
  render(<ArtifactList rows={[row('research:a', { title: 'Why slow?' }), row('research:b', { title: 'Sloppy code' }), row('research:c', { title: 'Old' })]} now={NOW} />);
  expect((await screen.findAllByRole('region')).map((r) => r.getAttribute('aria-label'))).toEqual(['Today', 'Yesterday', 'Fri, Nov 20']);
  const today = day('Today').getByRole('link');
  expect(today).toHaveTextContent('Why slow? Cold caches dominate 2:51 PM');
  expect(within(today).getByText('2:51 PM')).toHaveAttribute('datetime', at(22, 14, 51));
  expect(day('Yesterday').getByRole('link')).toHaveTextContent('12:05 AM');
});

test('a contract with a spec and an approval is one row that opens the spec, with one version and a mark', async () => {
  const key = 'contracts:x';
  serve(
    [art(key, 'contract.approved', 2, at(22, 11, 47), 'Approved version 10'), art(key, 'contract.spec', 20, at(22, 11, 8), 'Session API, version 10'), art(key, 'contract.spec', 19, at(22, 10), 'Session API, version 10')],
    [{ key, title: 'Session API', kind: 'rest', draft: 10, proposed: 10, approved: 10, pendingApproval: false }],
  );
  render(<ArtifactList rows={[row(key, { agent: 'contracts', title: 'Session API' })]} now={NOW} />);
  await listed();
  const link = await day('Today').findByRole('link', { name: /REST/ });
  expect(day('Today').getAllByRole('link')).toHaveLength(1);
  expect(link).toHaveAttribute('href', '#/s/contracts%3Ax/a/contract.spec');
  expect(link).toHaveTextContent('Session API REST v10 ✓ 11:47 AM');
  expect(within(link).getByRole('img', { name: 'Approved' })).toHaveTextContent('✓');
});

test('a day with two or more reviews folds them into one row that counts approvals and opens to list them; a lone review stays a row', async () => {
  serve([
    review(1, at(22, 14), 'Approve · 2 suggestions, 3 nits'),
    review(2, at(22, 13), 'Request changes · 1 blocking'),
    review(3, at(22, 12), 'Approve · no findings', 'dotagents'),
    review(4, at(21, 9), 'Comment · 1 nit, 2 questions'),
  ]);
  render(<ArtifactList rows={[]} now={NOW} />);
  await listed();
  const fold = await day('Today').findByRole('button', { name: /^3 pull request reviews quack, dotagents/ });
  expect(fold).toHaveAttribute('aria-expanded', 'false');
  expect(within(fold).getByRole('img', { name: '2 approved' })).toBeInTheDocument();
  expect(fold).toHaveTextContent(/2✓$/);
  expect(day('Today').queryAllByRole('link')).toHaveLength(0);
  await userEvent.click(fold);
  expect(fold).toHaveAttribute('aria-expanded', 'true');
  expect(day('Today').getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual([1, 2, 3].map((n) => `#/s/github%3Afagerbergj%2F${n === 3 ? 'dotagents' : 'quack'}%23${n}/a/github.review`));
  expect(day('Yesterday').queryByRole('button')).toBeNull();
  expect(day('Yesterday').getByRole('link')).toHaveTextContent('quack#4');
});

test('marks name the status for a screen reader: an approval, a change request, a draft, and none for a comment', async () => {
  serve(
    [review(1, at(22, 14), 'Approve · 1 nit'), review(2, at(21, 13), 'Request changes · 1 blocking'), review(3, at(20, 12), 'Comment · 1 question'), art('contracts:d', 'contract.spec', 31, at(22, 11), 'Judge API, version 31')],
    [{ key: 'contracts:d', title: 'Judge API', kind: 'typescript', draft: 31, pendingApproval: false }],
  );
  render(<ArtifactList rows={[row('contracts:d', { agent: 'contracts', title: 'Judge API' })]} now={NOW} />);
  await listed();
  expect(await day('Today').findByRole('img', { name: 'Draft, not yet proposed for approval' })).toBeInTheDocument();
  expect(day('Today').getByRole('img', { name: 'Approved' })).toBeInTheDocument();
  expect(day('Yesterday').getByRole('img', { name: 'Changes requested' })).toHaveTextContent('✗');
  expect(day('Fri, Nov 20').queryByRole('img')).toBeNull();
});

test('row text never shows a status word or says its version twice', async () => {
  serve(
    [review(1, at(22, 14), 'Approve · 1 nit'), review(2, at(21, 13), 'Request changes · 1 blocking'), art('contracts:d', 'contract.approved', 3, at(22, 11), 'Approved version 7'), art('contracts:d', 'contract.spec', 9, at(22, 10), 'Report artifact, version 7')],
    [{ key: 'contracts:d', title: 'Report artifact', kind: 'json-schema', draft: 7, proposed: 7, approved: 7, pendingApproval: false }],
  );
  render(<ArtifactList rows={[row('contracts:d', { agent: 'contracts', title: 'Report artifact' })]} now={NOW} />);
  await listed();
  await day('Today').findByText('JSON Schema');
  const text = screen.getAllByRole('link').map((l) => l.textContent ?? '');
  expect(text).toHaveLength(3);
  for (const t of text) expect(t).not.toMatch(/Approve|Request changes|Comment|Draft|version \d/i);
  expect(text.join(' ').match(/v7/g)).toHaveLength(1);
});

test('a review row shows its repo#number and a chip per finding count, or No findings', async () => {
  serve([review(2464, at(22, 14), 'Approve · 2 suggestions, 3 nits'), review(31, at(21, 8), 'Approve · no findings', 'home-server')]);
  render(<ArtifactList rows={[]} now={NOW} />);
  await listed();
  const link = await day('Today').findByRole('link');
  expect(within(link).getByText('quack#2464')).toBeInTheDocument();
  expect(within(link).getByText('2 suggestions')).toBeInTheDocument();
  expect(within(link).getByText('3 nits')).toBeInTheDocument();
  expect(day('Yesterday').getByRole('link')).toHaveTextContent(/home-server#31 No findings/);
});

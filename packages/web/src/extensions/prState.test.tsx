import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { App } from '../App.tsx';
import { Fail, FakeEventSource, fetchMock, routes, sess, stubViewport, useHarness } from '../test-harness.ts';

useHarness(() => ({ 'GET /agents': { agents: [] } }));

const pull = (n: number, over: object = {}) => ({ number: n, state: 'open', merged: false, merged_at: null, closed_at: null, html_url: `https://github.com/A/One/pull/${n}`, title: `fix: pr ${n}`, ...over });
const merged = { state: 'closed', merged: true, merged_at: '2026-11-02T00:00:00Z', closed_at: '2026-11-02T00:00:00Z' };
const mirror = (n: number) => `GET /apps/github/repos/a/one/pulls/${n}`;
const reads = () => fetchMock.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('apps/github/repos/'));
const badge = (n: number, state?: string) => screen.getByRole('link', { name: state ? `Pull request #${n}, ${state}, on GitHub` : `Pull request #${n} on GitHub` });
const findBadge = (n: number, state: string) => screen.findByRole('link', { name: `Pull request #${n}, ${state}, on GitHub` });

const stream = () => vi.waitFor(() => FakeEventSource.all[0] ?? Promise.reject(new Error('no stream yet')));

function list(keys: string[]) {
  routes['GET /sessions'] = { sessions: keys.map((k) => sess(k, { title: `fix: pr ${k.split('#')[1]}` })) };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
}

test('the PRs the list shows are read from the mirror once the stream opens, each by itself; each badge is its own link beside the row', async () => {
  routes[mirror(1)] = pull(1);
  routes[mirror(2)] = pull(2, merged);
  list(['github:A/One#1', 'github:a/one#2']);
  await screen.findByRole('link', { name: /#2 Pr 2/ });
  expect(FakeEventSource.all.map((s) => s.url)).toEqual(['apps/github/events']);
  expect(reads(), 'nothing is read before the stream is subscribed').toEqual([]);
  expect(badge(2)).toHaveAttribute('href', 'https://github.com/a/one/pull/2');
  FakeEventSource.all[0].open();
  const chip = await findBadge(2, 'merged');
  expect(chip).toHaveAttribute('href', 'https://github.com/A/One/pull/2');
  expect(chip).toHaveAttribute('target', '_blank');
  expect(chip).toHaveAttribute('rel', 'noopener noreferrer');
  expect(chip.closest('a[href^="#/s/"]')).toBeNull();
  expect(badge(1, 'open')).toHaveTextContent(/^#1$/);
  expect(reads().sort(), 'one read per PR, in GitHub\'s case-insensitive form').toEqual(['apps/github/repos/a/one/pulls/1', 'apps/github/repos/a/one/pulls/2']);
});

test('an archived review row is not read; its badge stays neutral and links by number', async () => {
  const live = sess('github:a/one#1', { title: 'fix: live' });
  const shelved = sess('github:a/one#2', { title: 'fix: shelved', state: 'archived' });
  const listed = (q: string) => `GET /sessions?${new URLSearchParams({ pageSize: '1000', q })}`;
  routes['GET /sessions'] = { sessions: [live, shelved] };
  routes[listed('NOT state:archived')] = { sessions: [live] };
  routes[listed('state:archived')] = { sessions: [shelved] };
  routes[mirror(1)] = pull(1);
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  (await stream()).open();
  await findBadge(1, 'open');
  await userEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await userEvent.selectOptions(screen.getByLabelText('Show'), 'Archived');
  expect(await screen.findByRole('link', { name: 'Pull request #2 on GitHub' })).toHaveAttribute('href', 'https://github.com/a/one/pull/2');
  expect(reads()).toEqual(['apps/github/repos/a/one/pulls/1']);
});

test('a pull_request event flips its badge at once, and a reopened stream re-reads without a flash', async () => {
  routes[mirror(1)] = pull(1);
  list(['github:a/one#1']);
  const es = await stream();
  es.open();
  await findBadge(1, 'open');
  // GitHub's casing of the repository still finds the row's PR.
  es.emit('pull_request', { action: 'closed', repository: { full_name: 'A/One' }, pull_request: pull(1, merged) });
  expect(badge(1, 'merged')).toBeInTheDocument();
  routes[mirror(1)] = pull(1, merged);
  es.open();
  expect(badge(1, 'merged'), 'the badge holds while the re-read is in flight').toBeInTheDocument();
  await vi.waitFor(() => expect(reads()).toHaveLength(2));
  expect(badge(1, 'merged')).toBeInTheDocument();
});

test('a failed read says so above the list; a badge already shown keeps its state', async () => {
  routes[mirror(1)] = pull(1, merged);
  list(['github:a/one#1']);
  const es = await stream();
  es.open();
  await findBadge(1, 'merged');
  routes[mirror(1)] = new Fail(502, { title: 'Bad Gateway', detail: 'GitHub is down' });
  es.open();
  expect(await screen.findByRole('alert')).toHaveTextContent('Pull request states are unavailable: GitHub is down');
  expect(badge(1, 'merged')).toBeInTheDocument();
});

test('a stream the app refuses (a 503 past its cap) is reopened after 30 s; the PRs it held back are read meanwhile', async () => {
  routes[mirror(1)] = pull(1);
  list(['github:a/one#1']);
  const es = await stream();
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    es.fail();
    vi.advanceTimersByTime(29_999);
    expect(FakeEventSource.all).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.all).toHaveLength(2);
  } finally {
    vi.useRealTimers();
  }
  expect(await findBadge(1, 'open')).toBeInTheDocument();
});

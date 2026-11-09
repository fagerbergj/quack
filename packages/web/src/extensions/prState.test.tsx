import { render, screen, within } from '@testing-library/react';
import { expect, test, vi } from 'vitest';
import { App } from '../App.tsx';
import { Fail, FakeEventSource, fetchMock, routes, sess, stubViewport, useHarness } from '../test-harness.ts';

useHarness(() => ({ 'GET /agents': { agents: [] } }));

const pull = (n: number, over: object = {}) => ({ number: n, state: 'open', merged: false, merged_at: null, closed_at: null, html_url: `https://github.com/A/One/pull/${n}`, title: `fix: pr ${n}`, ...over });
const merged = { state: 'closed', merged: true, merged_at: '2026-11-02T00:00:00Z', closed_at: '2026-11-02T00:00:00Z' };
const mirror = (n: number) => `GET /apps/github/repos/a/one/pulls/${n}`;
const reads = () => fetchMock.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('apps/github/repos/'));
const row = (n: number) => screen.getByRole('link', { name: new RegExp(`#${n} Pr ${n}`) });

const stream = () => vi.waitFor(() => FakeEventSource.all[0] ?? Promise.reject(new Error('no stream yet')));

function list(keys: string[]) {
  routes['GET /sessions'] = { sessions: keys.map((k) => sess(k, { title: `fix: pr ${k.split('#')[1]}` })) };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
}

test('the PRs the list shows are read from the mirror once the stream opens, each by itself', async () => {
  routes[mirror(1)] = pull(1);
  routes[mirror(2)] = pull(2, merged);
  list(['github:A/One#1', 'github:a/one#2']);
  await screen.findByRole('link', { name: /#2 Pr 2/ });
  expect(FakeEventSource.all.map((s) => s.url)).toEqual(['apps/github/events']);
  expect(reads(), 'nothing is read before the stream is subscribed').toEqual([]);
  FakeEventSource.all[0].open();
  expect(await within(row(2)).findByText('Merged')).toBeInTheDocument();
  expect(within(row(1)).getByText('Open')).toBeInTheDocument();
  expect(reads().sort(), 'one read per PR, in GitHub\'s case-insensitive form').toEqual(['apps/github/repos/a/one/pulls/1', 'apps/github/repos/a/one/pulls/2']);
});

test('a pull_request event flips its badge at once, and a reopened stream re-reads without a flash', async () => {
  routes[mirror(1)] = pull(1);
  list(['github:a/one#1']);
  const es = await stream();
  es.open();
  await within(await screen.findByRole('link', { name: /#1 Pr 1/ })).findByText('Open');
  // GitHub's casing of the repository still finds the row's PR.
  es.emit('pull_request', { action: 'closed', repository: { full_name: 'A/One' }, pull_request: pull(1, merged) });
  expect(within(row(1)).getByText('Merged')).toBeInTheDocument();
  routes[mirror(1)] = pull(1, merged);
  es.open();
  expect(within(row(1)).getByText('Merged'), 'the badge holds while the re-read is in flight').toBeInTheDocument();
  await vi.waitFor(() => expect(reads()).toHaveLength(2));
  expect(within(row(1)).getByText('Merged')).toBeInTheDocument();
});

test('a failed read says so above the list; a badge already shown keeps its state', async () => {
  routes[mirror(1)] = pull(1, merged);
  list(['github:a/one#1']);
  const es = await stream();
  es.open();
  await within(await screen.findByRole('link', { name: /#1 Pr 1/ })).findByText('Merged');
  routes[mirror(1)] = new Fail(502, { title: 'Bad Gateway', detail: 'GitHub is down' });
  es.open();
  expect(await screen.findByRole('alert')).toHaveTextContent('Pull request states are unavailable: GitHub is down');
  expect(within(row(1)).getByText('Merged')).toBeInTheDocument();
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
  expect(await within(row(1)).findByText('Open')).toBeInTheDocument();
});

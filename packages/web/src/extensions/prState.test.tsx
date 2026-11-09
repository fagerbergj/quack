import { render, screen, within } from '@testing-library/react';
import { expect, test } from 'vitest';
import { App } from '../App.tsx';
import { Fail, fetchMock, routes, sess, stubViewport, useHarness } from '../test-harness.ts';

useHarness(() => ({ 'GET /agents': { agents: [] } }));

const keys = (from: number, to: number) => Array.from({ length: to - from }, (_, i) => `github:a/one#${from + i}`);
const lookup = (k: string[]) => `GET /apps/github/pulls?keys=${encodeURIComponent(k.join(','))}`;
const merged = (k: string[]) => ({ pulls: Object.fromEntries(k.map((key) => [key.replace('github:', ''), { state: 'merged', at: '2026-11-02T00:00:00Z' }])) });

function list(all: string[]) {
  routes['GET /sessions'] = { sessions: all.map((k) => sess(k, { title: `fix: pr ${k.split('#')[1]}` })) };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
}

test('past 100 review sessions the PR states are looked up in batches the service accepts, and every row gets its chip', async () => {
  // The lookup sorts keys as strings, so the batches follow that order.
  const all = keys(1, 151).sort();
  routes[lookup(all.slice(0, 100))] = merged(all.slice(0, 100));
  routes[lookup(all.slice(100))] = merged(all.slice(100));
  list(all);
  expect(await screen.findByRole('link', { name: /#150 Pr 150/ })).toBeInTheDocument();
  await screen.findAllByText('Merged');
  expect(screen.getAllByText('Merged')).toHaveLength(150);
  expect(fetchMock.mock.calls.filter(([u]) => String(u).startsWith('apps/github/pulls'))).toHaveLength(2);
});

test('the PR chip is its own link to the pull request on GitHub, beside the row link rather than inside it', async () => {
  routes[lookup(keys(1, 3))] = { pulls: { 'a/one#1': { state: 'merged', at: '2026-11-02T00:00:00Z' } } };
  list(keys(1, 3));
  const chip = await screen.findByRole('link', { name: 'Pull request #1, merged, on GitHub' });
  expect(chip).toHaveAttribute('href', 'https://github.com/a/one/pull/1');
  expect(chip).toHaveAttribute('target', '_blank');
  expect(chip).toHaveAttribute('rel', 'noopener noreferrer');
  expect(chip.closest('a[href^="#/s/"]')).toBeNull();
  // A PR the service has no state for still links, by its number.
  expect(screen.getByRole('link', { name: '#2 on GitHub' })).toHaveAttribute('href', 'https://github.com/a/one/pull/2');
  expect(screen.getByRole('link', { name: /#1 Pr 1/ })).toHaveAttribute('href', '#/s/github%3Aa%2Fone%231');
});

test('a failed lookup says so above the list instead of dropping the chips silently', async () => {
  routes[lookup(keys(1, 3))] = new Fail(502, { title: 'Bad Gateway', detail: 'GitHub is down' });
  list(keys(1, 3));
  const alert = await screen.findByRole('alert');
  expect(alert).toHaveTextContent('Pull request states are unavailable: GitHub is down');
  expect(within(screen.getByRole('link', { name: /#1 Pr 1/ })).queryByText(/Open|Merged|Closed/)).toBeNull();
});

test('a lookup the service cut short is reported, not shown as complete', async () => {
  routes[lookup(keys(1, 3))] = { ...merged(keys(1, 2)), truncated: true };
  list(keys(1, 3));
  expect(await screen.findByRole('alert')).toHaveTextContent('answered only part of the lookup');
});

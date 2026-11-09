import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, routes, sess, snapshot, stubViewport, useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /artifacts': { artifacts: [] } }));

const REVIEW = 'github:fagerbergj/quack#2243';
// Reviews from two repos, so the list prefixes the repo; the header must not.
const SESSIONS = [
  sess(REVIEW, { title: 'fix(judge): Every failing item is named in the verdict', cost: { currencyCode: 'USD', amountMicros: '110000' } }),
  sess('github:acme/tool#5', { title: 'feat: add retries' }),
  sess('research:r1', { agent: 'researcher', title: 'What changed in the Langfuse v4 public ingestion API? Cite sources.' }),
  sess('home:main', { agent: 'quack', title: 'Plan the week' }),
  sess('sleeper:L1:8:lineup', { agent: 'sleeper' }),
  sess('contracts:c1', { agent: 'contracts', title: 'Sleeper schedule endpoint' }),
];

async function header(key: string) {
  routes['GET /sessions'] = { sessions: SESSIONS };
  routes[`GET /apps/github/pulls?keys=${encodeURIComponent(SESSIONS.filter((s) => s.key.startsWith('github:')).map((s) => s.key).sort().join(','))}`] = { pulls: { 'fagerbergj/quack#2243': { state: 'open', at: '2026-11-08T00:00:00Z' } } };
  routes[`GET /sessions/contracts%3Ac1/asks?${new URLSearchParams({ pageSize: '1000', q: 'type:question state:resolved' })}`] = { asks: [] };
  stubViewport(false);
  location.hash = `#/s/${encodeURIComponent(key)}`;
  render(<App />);
  FakeSocket.all[0]!.open();
  FakeSocket.all[0]!.frame(snapshot());
  // The session header's h1 is the one with the status line under it; a studio pane has statuses of its own.
  const statuses = await screen.findAllByRole('status');
  const line2 = statuses.map((s) => s.parentElement!).find((p) => p.previousElementSibling?.tagName === 'H1')!;
  return { line1: line2.previousElementSibling as HTMLElement, line2, bar: line2.parentElement!.parentElement! };
}

test('a review leads with its number and PR title; the repo, PR state and cost sit on line 2', async () => {
  const { line1, line2 } = await header(REVIEW);
  expect(line1).toHaveTextContent(/^#2243 Every failing item is named in the verdict$/);
  expect(line1).toHaveAttribute('title', 'Every failing item is named in the verdict');
  await screen.findByText('Open');
  expect(line2).toHaveTextContent(/^IdleOpenfagerbergj\/quack\$0\.11$/);
});

test.each([
  ['research:r1', 'What changed in the Langfuse v4 public ingestion API? Cite sources.'],
  ['home:main', 'quack'],
  ['sleeper:L1:8:lineup', 'Lineup · wk 8'],
  ['contracts:c1', 'Sleeper schedule endpoint'],
])('%s is headed by its own title, with its state on line 2', async (key, title) => {
  const { line1, line2 } = await header(key);
  expect(line1.textContent).toBe(title);
  expect(line2).toHaveTextContent(/^Idle/);
});

test('the bar holds only back, Details and the menu, all labelled icon buttons; the source link is a menu item', async () => {
  const { bar } = await header(REVIEW);
  const actions = [within(bar).getByRole('link', { name: 'Sessions' }), ...within(bar).getAllByRole('button')];
  expect(actions.map((el) => el.getAttribute('aria-label'))).toEqual(['Sessions', 'Details', 'More actions']);
  for (const el of actions) {
    expect(el).toHaveAttribute('title', el.getAttribute('aria-label'));
    expect(el).not.toHaveTextContent(/\S/);
  }
  await userEvent.click(within(bar).getByRole('button', { name: 'More actions' }));
  expect(within(bar).getByRole('menuitem', { name: 'Open on GitHub' })).toHaveAttribute('href', 'https://github.com/fagerbergj/quack/pull/2243');
});

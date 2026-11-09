import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { FakeEventSource, FakeSocket, routes, sess, snapshot, stubViewport, useHarness } from './test-harness.ts';

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
  routes['GET /apps/github/repos/fagerbergj/quack/pulls/2243'] = { number: 2243, state: 'open', merged: false, merged_at: null, closed_at: null, html_url: 'https://github.com/fagerbergj/quack/pull/2243', title: 't' };
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

test('a review leads with its PR title; its PR badge (state and number), repo and cost sit on line 2', async () => {
  const { line1, line2 } = await header(REVIEW);
  expect(line1).toHaveTextContent(/^#2243 Every failing item is named in the verdict$/);
  expect(within(line1).getByText('#2243'), 'the badge shows the number; line 1 keeps it for screen readers').toHaveClass('sr-only');
  expect(line1).toHaveAttribute('title', 'Every failing item is named in the verdict');
  FakeEventSource.all.at(-1)!.open();
  expect(await within(line2).findByRole('link', { name: 'Pull request #2243, open, on GitHub' })).toHaveAttribute('href', 'https://github.com/fagerbergj/quack/pull/2243');
  expect(line2).toHaveTextContent(/^Idle#2243fagerbergj\/quack\$0\.11$/);
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

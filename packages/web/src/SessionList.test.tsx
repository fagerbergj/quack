import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { RECENT_CAP } from './SessionList.tsx';
import { routes, sess, stubViewport, useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /artifacts': { artifacts: [] } }));

// Newest first, as the server orders them.
const many = (n: number) => Array.from({ length: n }, (_, i) => sess(`gh:s${i}`, { title: i === 100 ? 'Needle' : `Session ${i}`, lastActivity: new Date(Date.UTC(2026, 10, 1) - i * 60_000).toISOString() }));

async function open() {
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  return screen.findByRole('region', { name: 'Recent' });
}

test('Recent renders the newest 50 and a button expands it in place', async () => {
  routes['GET /sessions'] = { sessions: many(120) };
  const recent = await open();
  expect(within(recent).getAllByRole('link')).toHaveLength(RECENT_CAP);
  const more = within(recent).getByRole('button', { name: 'Show 70 more' });
  expect(more).toHaveAttribute('aria-expanded', 'false');
  await userEvent.click(more);
  expect(within(recent).getAllByRole('link')).toHaveLength(120);
  const fewer = within(recent).getByRole('button', { name: 'Show fewer' });
  expect(fewer).toHaveAttribute('aria-expanded', 'true');
  await userEvent.click(fewer);
  expect(within(recent).getAllByRole('link')).toHaveLength(RECENT_CAP);
});

test('a list within the cap has no button', async () => {
  routes['GET /sessions'] = { sessions: many(RECENT_CAP) };
  const recent = await open();
  expect(within(recent).queryByRole('button')).toBeNull();
});

test('search finds a session past the cap', async () => {
  const rows = many(120);
  routes['GET /sessions'] = (url: string) => ({ sessions: new URL(url, 'http://x').searchParams.get('q')?.includes('Needle') ? [rows[100]] : rows });
  const recent = await open();
  expect(within(recent).queryByRole('link', { name: /Needle/ })).toBeNull();
  // The search debounces on a timer; a fake clock steps past it instead of waiting on the real one.
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    fireEvent.change(screen.getByRole('searchbox', { name: 'Search sessions' }), { target: { value: 'Needle' } });
    act(() => void vi.advanceTimersByTime(250));
  } finally {
    vi.useRealTimers();
  }
  expect(await screen.findByRole('link', { name: /Needle/ }, { timeout: 5000 })).toBeInTheDocument();
  expect(within(await screen.findByRole('region', { name: 'Recent' })).getAllByRole('link')).toHaveLength(1);
});

test('an interrupted row says why in its tooltip and to screen readers', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:cut', { title: 'Cut off', lastActivity: '2026-11-02T01:00:00.000Z' })] };
  const recent = await open();
  const why = 'The last run stopped before it finished; it resumes on the next message';
  expect(within(recent).getByTitle(why)).toHaveTextContent('Interrupted');
  expect(within(recent).getByRole('link')).toHaveAccessibleName(expect.stringContaining(why));
});

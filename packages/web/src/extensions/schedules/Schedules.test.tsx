import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, test, vi } from 'vitest';
import type { Row } from '../../sessions.ts';
import { Schedules } from './Schedules.tsx';

// Local wall times, so the day labels hold in any test-runner zone.
const at = (day: number, h: number, m: number) => new Date(2026, 10, day, h, m).toISOString();
const s = (id: string, over: object = {}) => ({ id, agent: 'sleeper-digest', sessionKey: id, input: 'go', times: [at(24, 9, 0)], lastRun: null, nextRun: at(24, 9, 0), missedRuns: [], ...over });
const stub = (res: () => Response) => vi.stubGlobal('fetch', vi.fn(async () => res()));
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

// A list served from `list`; a DELETE answers `deleted` and drops the schedule from later reads.
function server(list: ReturnType<typeof s>[], deleted = () => new Response(null, { status: 204 })) {
  const calls: string[] = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push(`${init?.method ?? 'GET'} ${url}`);
    if (init?.method !== 'DELETE') return new Response(JSON.stringify({ schedules: list }));
    const res = deleted();
    if (res.ok) list = list.filter((x) => `apps/schedules/${encodeURIComponent(x.id)}` !== url);
    return res;
  }));
  return { deletes: () => calls.filter((c) => c.startsWith('DELETE')) };
}

const deleteVia = (title: string) => {
  fireEvent.click(screen.getByRole('button', { name: `More for ${title}` }));
  fireEvent.click(within(screen.getByRole('menu', { name: `More for ${title}` })).getByRole('menuitem', { name: 'Delete schedule' }));
};

test('rows group under their local day, soonest first, and a run in the same minute shows its time once', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date(2026, 10, 22, 12, 0));
  stub(() => new Response(JSON.stringify({ schedules: [
    s('late', { nextRun: at(24, 15, 0) }), s('none', { nextRun: null }), s('b', { nextRun: at(24, 1, 15) }), s('a', { nextRun: at(23, 4, 30) }), s('c', { nextRun: at(24, 1, 15) }),
  ] })));
  render(<Schedules rows={undefined} />);
  const tomorrow = await screen.findByRole('region', { name: 'Mon, Nov 23, Tomorrow' });
  expect(within(tomorrow).getByText('Tomorrow')).toBeInTheDocument();
  expect(within(tomorrow).getByText('4:30 AM')).toBeVisible();
  const tue = screen.getByRole('region', { name: 'Tue, Nov 24' });
  expect(within(tue).getAllByRole('link').map((l) => l.textContent)).toEqual(['b', 'c', 'late']);
  expect(within(tue).getAllByText('1:15 AM').map((t) => t.className)).toEqual(['', 'sr-only']);
  expect(within(screen.getByRole('region', { name: 'No upcoming run' })).getByRole('link')).toHaveTextContent('none');
});

test('a schedule is titled by its session, falls back to the agent for a new session each run, and repeats only for cron', async () => {
  const rows = [{ key: 'weekly', agent: 'sleeper-digest', title: 'Weekly plan', failures: [] }] as unknown as Row[];
  stub(() => new Response(JSON.stringify({ schedules: [
    s('sleeper/digest/recap', { sessionKey: 'sleeper:L1:11:digest' }),
    s('weekly', { cron: '0 9 * * 1', tz: 'America/New_York', times: undefined, lastRun: '2026-11-02T14:00:00Z', missedRuns: ['x', 'y'] }),
    s('daily', { agent: 'morning-note', sessionKey: 'd-{time}', cron: '30 6 * * *', times: undefined }),
  ] })));
  render(<Schedules rows={rows} />);
  expect(await screen.findByRole('link', { name: 'Week 11 recap' })).toHaveAttribute('href', '#/s/sleeper%3AL1%3A11%3Adigest');
  expect(screen.queryByText('sleeper/digest/recap')).toBeNull();
  expect(screen.getByRole('link', { name: 'Weekly plan' })).toHaveAttribute('href', '#/s/weekly');
  expect(screen.getByText('Every Monday at 9:00 America/New_York')).toBeInTheDocument();
  expect(screen.getByText(/last ran Nov 2/)).toBeInTheDocument();
  expect(screen.getByText('2 missed')).toBeInTheDocument();
  expect(screen.getByText('morning-note')).toBeInTheDocument();
  expect(screen.getByText(/New session each run/)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'More for morning-note' })).toBeInTheDocument();
  expect(screen.getAllByText(/^Every /)).toHaveLength(2);
  expect(screen.queryByText(/Once/)).toBeNull();
  expect(screen.queryByText(/has not run yet/)).toBeNull();
});

test('Delete sits in the row menu beside Open session; a run with no single session has no Open session', async () => {
  server([s('weekly'), s('daily', { sessionKey: 'd-{time}', agent: 'morning-note' })]);
  render(<Schedules rows={undefined} />);
  await screen.findByRole('link', { name: 'weekly' });
  expect(screen.queryByRole('button', { name: /Delete/ })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'More for weekly' }));
  const items = within(screen.getByRole('menu', { name: 'More for weekly' })).getAllByRole('menuitem');
  expect(items.map((i) => i.textContent)).toEqual(['Open session', 'Delete schedule']);
  expect(items[0]).toHaveAttribute('href', '#/s/weekly');
  fireEvent.click(screen.getByRole('button', { name: 'More for morning-note' }));
  expect(within(screen.getByRole('menu', { name: 'More for morning-note' })).getAllByRole('menuitem').map((i) => i.textContent)).toEqual(['Delete schedule']);
});

test('Delete hides the row at once, Undo brings it back with nothing sent, and a lapsed Undo sends one DELETE', async () => {
  const api = server([s('weekly'), s('daily')]);
  render(<Schedules rows={undefined} />);
  await screen.findByRole('link', { name: 'daily' });
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  deleteVia('daily');
  expect(screen.queryByRole('link', { name: 'daily' })).toBeNull();
  expect(screen.getByRole('status')).toHaveTextContent('Deleted daily');
  fireEvent.click(screen.getByRole('button', { name: 'Undo' }));
  expect(screen.getByRole('link', { name: 'daily' })).toBeInTheDocument();
  await act(() => vi.advanceTimersByTimeAsync(10_000));
  expect(api.deletes()).toEqual([]);

  deleteVia('daily');
  await act(() => vi.advanceTimersByTimeAsync(4_999));
  expect(api.deletes()).toEqual([]);
  await act(() => vi.advanceTimersByTimeAsync(1));
  expect(api.deletes()).toEqual(['DELETE apps/schedules/daily']);
  await act(() => vi.advanceTimersByTimeAsync(10_000));
  expect(api.deletes()).toHaveLength(1);
  expect(screen.queryByRole('status')).toBeNull();
  expect(screen.queryByRole('link', { name: 'daily' })).toBeNull();
  expect(screen.getByRole('link', { name: 'weekly' })).toBeInTheDocument();
});

test('leaving the page sends the pending DELETE, and a failed one brings the row back with its cause', async () => {
  const api = server([s('weekly')], () => new Response('{"error":"db down"}', { status: 503 }));
  const view = render(<Schedules rows={undefined} />);
  await screen.findByRole('link', { name: 'weekly' });
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  deleteVia('weekly');
  await act(() => vi.advanceTimersByTimeAsync(5_000));
  await vi.waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('db down'));
  expect(screen.getByRole('link', { name: 'weekly' })).toBeInTheDocument();

  deleteVia('weekly');
  view.unmount();
  expect(api.deletes()).toHaveLength(2);
  // That failure landed after the page went away, so the next visit names it beside the row it left.
  await act(() => vi.advanceTimersByTimeAsync(0));
  vi.useRealTimers();
  render(<Schedules rows={undefined} />);
  expect(await screen.findByRole('alert')).toHaveTextContent('Could not delete weekly: db down');
  expect(await screen.findByRole('link', { name: 'weekly' })).toBeInTheDocument();
});

test('a second delete inside the Undo window sends the first at once and holds the second', async () => {
  const api = server([s('weekly'), s('daily')]);
  render(<Schedules rows={undefined} />);
  await screen.findByRole('link', { name: 'daily' });
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  deleteVia('daily');
  deleteVia('weekly');
  expect(api.deletes()).toEqual(['DELETE apps/schedules/daily']);
  await act(() => vi.advanceTimersByTimeAsync(5_000));
  expect(api.deletes()).toEqual(['DELETE apps/schedules/daily', 'DELETE apps/schedules/weekly']);
});

test('closing or reloading the tab mid-countdown sends the DELETE once, as a request that outlives the page', async () => {
  const inits: Array<RequestInit | undefined> = [];
  const api = server([s('weekly')]);
  const served = globalThis.fetch;
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => (init?.method === 'DELETE' && inits.push(init), served(url, init))));
  const view = render(<Schedules rows={undefined} />);
  await screen.findByRole('link', { name: 'weekly' });
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  deleteVia('weekly');
  dispatchEvent(new Event('pagehide'));
  expect(api.deletes()).toEqual(['DELETE apps/schedules/weekly']);
  expect(inits[0]?.keepalive).toBe(true);
  await act(() => vi.advanceTimersByTimeAsync(10_000));
  view.unmount();
  expect(api.deletes()).toHaveLength(1);
});

test('a delete moves focus to Undo, since the row that held the menu is gone', async () => {
  server([s('weekly')]);
  render(<Schedules rows={undefined} />);
  await screen.findByRole('link', { name: 'weekly' });
  deleteVia('weekly');
  expect(screen.getByRole('button', { name: 'Undo' })).toHaveFocus();
});

test('the list follows its pages', async () => {
  const calls: string[] = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    calls.push(url);
    const token = new URL(url, 'http://x').searchParams.get('pageToken');
    return new Response(JSON.stringify(token ? { schedules: [s('daily')] } : { schedules: [s('weekly')], nextPageToken: 'p2' }));
  }));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByRole('link', { name: 'daily' })).toBeInTheDocument();
  expect(calls.some((c) => c.includes('pageToken=p2'))).toBe(true);
});

test('an empty list says so', async () => {
  stub(() => new Response('{"schedules":[]}'));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByText('No schedules yet.')).toBeInTheDocument();
});

test('a 503 names the cause', async () => {
  stub(() => new Response('{"error":"db down"}', { status: 503 }));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByRole('alert')).toHaveTextContent('Schedules is unavailable: db down');
});

test('an unregistered app says schedules is not connected', async () => {
  stub(() => new Response('{"error":"no such app"}', { status: 404 }));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByRole('alert')).toHaveTextContent('Schedules is not connected.');
});

test('a network failure names the cause', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch'); }));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByRole('alert')).toHaveTextContent('Schedules is unavailable: Failed to fetch');
});

test('one set time says nothing more, several say how many are coming', async () => {
  server([s('once'), s('thrice', { times: [at(24, 9, 0), at(25, 9, 0), at(26, 9, 0)] })]);
  render(<Schedules rows={undefined} />);
  const thrice = (await screen.findByRole('link', { name: 'thrice' })).closest('li')!;
  expect(thrice).toHaveTextContent('sleeper-digest · 3 set times');
  expect(screen.getByRole('link', { name: 'once' }).closest('li')).not.toHaveTextContent(/set time|Once/);
});

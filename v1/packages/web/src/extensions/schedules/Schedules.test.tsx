import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import { Schedules } from './Schedules.tsx';

const s = (id: string, over: object = {}) => ({ id, agent: 'sleeper', sessionKey: 'weekly', input: 'go', cron: '0 9 * * 1', tz: 'America/New_York', source: 'agent', lastRun: null, nextRun: '2026-11-09T14:00:00Z', missedRuns: [], ...over });
const stub = (res: () => Response) => vi.stubGlobal('fetch', vi.fn(async () => res()));
afterEach(() => vi.unstubAllGlobals());

test('each schedule reads in plain words with its session link and last run', async () => {
  stub(() => new Response(JSON.stringify({ schedules: [s('sleeper/weekly', { lastRun: '2026-11-02T14:00:00Z', missedRuns: ['a', 'b'] }), s('daily', { source: 'api', sessionKey: 'd-{time}', cron: '30 6 * * *' })] })));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByText('Every Monday at 9:00 America/New_York')).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'weekly' })).toHaveAttribute('href', '#/s/weekly');
  expect(screen.getByText(/last ran Nov 2/)).toBeInTheDocument();
  expect(screen.getByText('2 missed')).toBeInTheDocument();
  expect(screen.getByText('Every day at 6:30 America/New_York')).toBeInTheDocument();
  expect(screen.getByText(/in a new session each time/)).toBeInTheDocument();
  expect(screen.getByText(/has not run yet/)).toBeInTheDocument();
});

test('only API schedules offer Delete, and it reloads the list', async () => {
  const calls: string[] = [];
  let list = [s('sleeper/weekly'), s('daily', { source: 'api' })];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push(`${init?.method ?? 'GET'} ${url}`);
    if (init?.method === 'DELETE') list = list.filter((x) => x.id !== 'daily');
    return new Response(JSON.stringify({ schedules: list }));
  }));
  render(<Schedules rows={undefined} />);
  await screen.findByText('sleeper/weekly');
  expect(screen.getAllByRole('button')).toHaveLength(1);
  await userEvent.click(screen.getByRole('button', { name: 'Delete daily' }));
  await vi.waitFor(() => expect(screen.queryByText('daily')).toBeNull());
  expect(calls).toContain('DELETE apps/schedules/daily');
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

test('a network failure names the cause', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch'); }));
  render(<Schedules rows={undefined} />);
  expect(await screen.findByRole('alert')).toHaveTextContent('Schedules is unavailable: Failed to fetch');
});

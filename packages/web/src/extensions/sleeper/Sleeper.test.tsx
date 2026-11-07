import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import type { Session } from '../../api.ts';
import { rowOf, type Row } from '../../sessions.ts';
import { sess } from '../../test-harness.ts';
import { Sleeper } from './Sleeper.tsx';
import digest7 from './fixtures/7-digest.json';
import retro7 from './fixtures/7-retro.json';
import history8 from './fixtures/8-history.json';
import lineup8 from './fixtures/8-lineup.json';
import finder8 from './fixtures/8-trade-finder.json';
import trends8 from './fixtures/8-trends.json';
import waivers8 from './fixtures/8-waivers.json';

// Real sealed results from the league's own sessions; week 9's lineup is still running and has none yet.
const L = '1356407594683482112';
const SEALED: Record<string, object> = { '8:lineup': lineup8, '8:waivers': waivers8, '8:trade-finder': finder8, '8:trends': trends8, '8:history': history8, '7:digest': digest7, '7:retro': retro7 };
const key = (week: number, job: string) => `sleeper:${L}:${week}:${job}`;
const row = (week: number, job: string, running = false) => rowOf(sess(key(week, job), { agent: 'a', state: running ? 'running' : 'idle' }) as Session, false);
// A sealed fixture as the Sessions API lists it: dates as RFC 3339, and no verdict, which is the judge's.
const listed = (session: string, a: { sealedAt: number } & Record<string, unknown>) =>
  ({ session, kind: a.kind, version: a.version, hash: a.hash, content: a.content, sealedAt: new Date(a.sealedAt).toISOString(), summary: a.summary });
const ALL = [...Object.keys(SEALED).map((k) => row(Number(k.split(':')[0]), k.split(':')[1]!)), row(9, 'lineup', true)];
// Sunday Nov 1 15:00 UTC is "now"; these are the service's next runs (Eastern times in the comments).
const RUNS = [{ job: 'digest', at: '2026-11-03T06:15:00.000Z' }, { job: 'retro', at: '2026-11-03T06:15:00.000Z' }, { job: 'waivers', at: '2026-11-03T20:00:00.000Z' }, { job: 'trade-finder', at: '2026-11-04T10:00:00.000Z' }, { job: 'trends', at: '2026-11-02T09:30:00.000Z' }];
const NOW = () => Date.parse('2026-11-01T15:00:00Z');

let live: number | undefined;
// What the server answers a league filter with; the page asks for its league's sessions instead of reading the workspace's rows.
let served: Row[] = ALL;
const calls: Array<[string, RequestInit | undefined]> = [];
beforeEach(() => {
  calls.length = 0;
  live = 9;
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push([url, init]);
    if (init?.method === 'POST') return new Response(JSON.stringify({ sessionKey: key(9, 'waivers') }));
    if (url === 'apps/sleeper/schedule') return new Response(JSON.stringify({ week: live, runs: RUNS }));
    if (url === 'apps/sleeper') return new Response(JSON.stringify({ league: L }));
    if (url.startsWith('sessions?')) return new Response(JSON.stringify({ sessions: served }));
    const m = /^sessions\/([^/?]+)\/artifacts\?/.exec(url);
    const session = m ? decodeURIComponent(m[1]!) : '';
    const sealed = SEALED[session.split(':').slice(2, 4).join(':')] as { sealedAt: number } | undefined;
    return new Response(JSON.stringify({ artifacts: sealed ? [listed(session, sealed)] : [] }));
  }));
});
afterEach(() => vi.unstubAllGlobals());

const page = (rows = ALL) => ((served = rows), render(<Sleeper rows={rows} now={NOW} />));
const card = async (name: string) => within(await screen.findByRole('article', { name }));

test('a card shows as soon as its own read lands, while a slow session is still booting', async () => {
  const stub = vi.mocked(fetch);
  const real = stub.getMockImplementation()!;
  stub.mockImplementation((url, init) => (String(url).includes(encodeURIComponent(':history')) ? new Promise<Response>(() => {}) : real(url, init)));
  page();
  expect(await screen.findByRole('article', { name: 'Waivers' })).toBeInTheDocument();
  expect(screen.queryByRole('article', { name: 'League history' })).toBeNull();
});

test('the current week is one scroll: the lineup, then each job\'s latest result with Open', async () => {
  page();
  expect(await screen.findByRole('heading', { name: 'Week 9' })).toBeInTheDocument();
  expect(await screen.findByText('Substation Supremacy')).toBeInTheDocument();
  expect(await screen.findByText('Running now')).toBeInTheDocument();
  for (const name of ['Waivers', 'Trades', 'Trends', 'Recap · week 7', 'Retro · week 7', 'League history']) {
    expect(await screen.findByRole('article', { name })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: `Open ${name}` })).toBeInTheDocument();
  }
  expect(screen.getByRole('button', { name: 'Later week' })).toBeDisabled();
  expect(document.body).not.toHaveTextContent(/\bcard\b|\bv\d+\b|version|sleeper\.cards|Waiver claims/i);
});

test("the current week's lineup shows the live card and when it locks", async () => {
  live = 8;
  page(ALL.filter((r) => !r.key.endsWith(':9:lineup')));
  const lineup = await card('Week 8 lineup');
  expect(lineup.getByText('Joe Burrow')).toBeInTheDocument();
  expect(lineup.getByText(/Bench · /)).toBeInTheDocument();
  expect(screen.getByRole('region', { name: 'This week' })).toHaveTextContent(/Lineup lock(s|ed)Sun/);
  expect(screen.getByLabelText('Today').closest('section')).toHaveTextContent('Sun');
});

test('waivers open to ranked claims, who to drop, why and the news', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open Waivers' }));
  const w = await card('Waivers');
  expect(w.getByText('+ Pat Freiermuth')).toBeInTheDocument();
  expect(w.getAllByText(/− Jordyn Tyson/).length).toBeGreaterThan(0);
  expect(w.getByText(/Rolling · priority 8 of 10/)).toBeInTheDocument();
  expect(w.getByText(/Freiermuth sits wk 9 but starts wk 10/)).toBeInTheDocument();
  expect(w.getByText('52 more players checked')).toBeInTheDocument();
  expect(w.getByText('News (2)')).toBeInTheDocument();
  expect(w.getByRole('link', { name: 'Ask about this' })).toHaveAttribute('href', `#/s/${encodeURIComponent(key(8, 'waivers'))}`);
});

test('trades with no deal says so in the short card and when opened', async () => {
  page();
  const t = await card('Trades');
  expect(t.getByText('No deals: no partner has a surplus player this team needs.')).toBeInTheDocument();
  expect(t.getByText('No deal this week')).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: 'Open Trades' }));
  expect(t.getByRole('button', { name: 'Close Trades' })).toBeInTheDocument();
});

test('trends group by kind, name the player and carry no player id', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open Trends' }));
  const t = await card('Trends');
  expect(t.getByText('Injuries')).toBeInTheDocument();
  expect(t.getByText('Rising on waivers')).toBeInTheDocument();
  expect(t.getByText('Devin Singletary · RB · NYG')).toBeInTheDocument();
  expect(t.getByText(/Devin Singletary leads free-agent adds/)).toBeInTheDocument();
  expect(document.body).not.toHaveTextContent(/6130|4881|7547/);
  expect(t.getByText('Source notes')).toBeInTheDocument();
});

test('the short recap previews your game first, then the biggest margin', async () => {
  page();
  const r = await card('Recap · week 7');
  expect(r.getByText('I Think I’’m Gonna Puka 99.68 · Substation Supremacy 140.28')).toBeInTheDocument();
});

test('the recap opens to every game and the standings with the playoff line', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open Recap · week 7' }));
  const r = await card('Recap · week 7');
  expect(r.getByText('140.28')).toBeInTheDocument();
  expect(r.getAllByText('Rice Cooker').length).toBeGreaterThan(0);
  expect(r.getByText('Standings · top 6 make it')).toBeInTheDocument();
  expect(r.getByText('Final scores from Sleeper, week 7.')).toBeInTheDocument();
});

test('the retro opens to figures, lineup and waiver misses and lessons', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open Retro · week 7' }));
  const r = await card('Retro · week 7');
  expect(r.getByText('scored · won')).toBeInTheDocument();
  expect(r.getByText('165.08')).toBeInTheDocument();
  expect(r.getByText('Jameson Williams over Justin Jefferson')).toBeInTheDocument();
  expect(r.getByText('Tampa Bay Buccaneers over New England Patriots')).toBeInTheDocument();
  expect(r.getAllByText('Not knowable').length).toBeGreaterThan(0);
  expect(r.getByText('Lessons')).toBeInTheDocument();
});

test('league history opens to every season with its record and draft', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open League history' }));
  const h = await card('League history');
  expect(h.getByText('In progress')).toBeInTheDocument();
  expect(h.getByText('2025')).toBeInTheDocument();
  expect(h.getByText(/Champion The Olave Garden/)).toBeInTheDocument();
  expect(h.getByText(/Draft 6 steals · 2 reaches/)).toBeInTheDocument();
});

test('the week stepper goes back, and a past week shows only results that exist', async () => {
  page();
  await screen.findByRole('article', { name: 'Waivers' });
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  expect(screen.getByRole('heading', { name: 'Week 8' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Later week' })).toBeEnabled();
  await userEvent.click(screen.getByRole('button', { name: 'Later week' }));
  expect(screen.getByRole('heading', { name: 'Week 9' })).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  expect(screen.getByRole('button', { name: 'This week' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Run a job' })).toBeNull();
  for (const name of ['Start / sit', 'Waivers', 'Trades', 'Trends', 'League history']) expect(await screen.findByRole('article', { name })).toBeInTheDocument();
  expect(screen.queryByRole('article', { name: /Recap|Retro/ })).toBeNull();
  // Week 8 has no retro and its history row does not exist yet, so there is no result block.
  expect(screen.queryByRole('region', { name: 'Week 8 result' })).toBeNull();
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  expect(screen.getByRole('button', { name: 'Earlier week' })).toBeDisabled();
});

test('a past week leads with how it ended, then its own results, each with Open', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Earlier week' }));
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  const result = await screen.findByRole('region', { name: 'Week 7 result' });
  expect(result).toHaveTextContent('Won');
  expect(result).toHaveTextContent('140.28 to 99.68');
  expect(result).toHaveTextContent('Left 24.8 on the bench (best lineup 165.08)');
  expect(screen.getAllByRole('article').map((a) => a.getAttribute('aria-label'))).toEqual(['Week 7 recap', 'Week 7 retro']);
  await userEvent.click(screen.getByRole('button', { name: 'Open Week 7 retro' }));
  expect(await screen.findByText('Lessons')).toBeInTheDocument();
});

test('the result falls back to the history job when the week has no retro', async () => {
  page(ALL.filter((r) => !r.key.endsWith(':7:retro')));
  await userEvent.click(await screen.findByRole('button', { name: 'Earlier week' }));
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  const result = await screen.findByRole('region', { name: 'Week 7 result' });
  expect(result).toHaveTextContent(/(Won|Lost)\s*140.28 to 99.68/);
  expect(result).toHaveTextContent(/Left \d+(\.\d+)? on the bench/);
});

test('This week returns to the current week', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Earlier week' }));
  await userEvent.click(screen.getByRole('button', { name: 'This week' }));
  expect(await screen.findByRole('heading', { name: 'Week 9' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Later week' })).toBeDisabled();
  expect(screen.getByRole('button', { name: 'Run a job' })).toBeInTheDocument();
});

test('Run a job lists every job with its last result and starts one through the proxy route', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Run a job' }));
  const sheet = within(await screen.findByRole('dialog', { name: 'Run a job' }));
  expect(sheet.getAllByRole('button', { name: /^Run / })).toHaveLength(7);
  expect(sheet.getByText('Running now')).toBeInTheDocument();
  expect(sheet.getByRole('button', { name: 'Run Lineup' })).toBeDisabled();
  expect(sheet.getByText(/Recap · week 7 · Sun/)).toBeInTheDocument();
  expect(sheet.getAllByText(/next Tue 1:15 AM/)).toHaveLength(2);
  await userEvent.click(sheet.getByRole('button', { name: 'Run Waivers' }));
  expect(await sheet.findByRole('status')).toHaveTextContent('Waivers started');
  const post = calls.find(([, i]) => i?.method === 'POST')!;
  expect(post[0]).toBe('apps/sleeper/waivers');
  expect(post[1]?.headers).toBeUndefined();
});

test('a refused Run shows the reason, and tapping a job name closes the sheet at its card', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Run a job' }));
  vi.mocked(fetch).mockImplementationOnce(async () => new Response(JSON.stringify({ error: 'on-demand jobs are not configured' }), { status: 501 }));
  await userEvent.click(screen.getByRole('button', { name: 'Run Trends' }));
  expect(await screen.findByRole('status')).toHaveTextContent('Could not start: on-demand jobs are not configured');
  const scroll = vi.fn();
  Element.prototype.scrollIntoView = scroll;
  await userEvent.click(screen.getByRole('button', { name: /^Trends/ }));
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(scroll).toHaveBeenCalled();
});

test('the page asks the server for its league by key prefix, following pages, and ignores other leagues', async () => {
  const other = { ...row(9, 'lineup'), key: 'sleeper:999:9:lineup' };
  vi.mocked(fetch).mockImplementation(async (url, init) => {
    const u = String(url);
    calls.push([u, init]);
    if (u === 'apps/sleeper') return new Response(JSON.stringify({ league: L }));
    if (u === 'apps/sleeper/schedule') return new Response(JSON.stringify({ week: undefined, runs: [] }));
    if (!u.startsWith('sessions?')) return new Response(JSON.stringify({ artifacts: [] }));
    return new Response(JSON.stringify(new URL(u, 'http://x').searchParams.get('pageToken') ? { sessions: [row(9, 'lineup', true)] } : { sessions: [other, row(8, 'waivers')], nextPageToken: 't' }));
  });
  page();
  expect(await screen.findByRole('heading', { name: 'Week 9' })).toBeInTheDocument();
  const asked = calls.map(([u]) => new URL(u, 'http://x')).filter((u) => u.pathname === '/sessions');
  // q is Lucene: the key's colons are escaped and the trailing * is the prefix wildcard.
  expect(asked.map((u) => u.searchParams.get('q'))).toEqual([`key:sleeper\\:${L}\\:*`, `key:sleeper\\:${L}\\:*`]);
  expect(asked.map((u) => u.searchParams.has('filter'))).toEqual([false, false]);
  expect(calls.some(([u]) => u.includes('sleeper%3A999'))).toBe(false);
});

test('without a league the page says it is not set up', async () => {
  vi.mocked(fetch).mockImplementation(async () => new Response('{}', { status: 501 }));
  page([]);
  expect(await screen.findByText('Sleeper is not set up on this server.')).toBeInTheDocument();
});

test('a league with no sessions invites the first run', async () => {
  live = undefined;
  page([]);
  expect(await screen.findByText('No results yet. Run a job from the menu.')).toBeInTheDocument();
});

test('the week strip lists the runs coming this week, grouped by time, and when the lineup locks', async () => {
  live = 8;
  page(ALL.filter((r) => !r.key.endsWith(':9:lineup')));
  const strip = await screen.findByRole('region', { name: 'This week' });
  expect(await within(strip).findByText('Recap and retro')).toBeInTheDocument();
  expect(within(strip).getByText('Waivers process')).toBeInTheDocument();
  expect(within(strip).getByText('Trade finder')).toBeInTheDocument();
  expect(within(strip).queryByText(/Trends/)).toBeNull();
  // The lock comes from the lineup's artifact, which lands after the schedule.
  expect(await within(strip).findByText(/Lineup lock/)).toBeInTheDocument();
  expect(within(strip).getByRole('img', { name: 'Today' }).tagName.toLowerCase()).toBe('svg');
});

test('the week is a timeline: the duck faces right, runs sit at their time, the list is time-sorted with stable colors', async () => {
  live = 8;
  page(ALL.filter((r) => !r.key.endsWith(':9:lineup')));
  const strip = await screen.findByRole('region', { name: 'This week' });
  await within(strip).findByText(/Lineup lock/);
  expect(within(strip).getByRole('img', { name: 'Today' })).toHaveStyle({ transform: 'scaleX(-1)' });
  expect(strip.querySelector('[data-marker="lock"]')).not.toBeNull();
  const dots = [...strip.querySelectorAll<HTMLElement>('[data-marker="run"]')];
  const lefts = dots.map((d) => parseFloat(d.style.left));
  expect(lefts).toHaveLength(3);
  expect(lefts).toEqual([...lefts].sort((a, b) => a - b));
  // Tue 1:15 AM is next week's Tuesday: past the first day, not in today's column.
  expect(lefts[0]).toBeGreaterThan(100 / 7);
  const rows = within(strip).getAllByRole('listitem').map((li) => li.textContent);
  expect(rows[0]).toMatch(/Lineup lock.*Sun/);
  expect(rows.slice(1).map((r) => r!.replace(/.*(Tue|Wed).*/, '$1'))).toEqual(['Tue', 'Tue', 'Wed']);
  const tone = (name: string) => within(strip).getByText(name).previousElementSibling!.className.match(/bg-\w+/)![0];
  expect(tone('Waivers process')).toBe('bg-warning');
  expect(tone('Trade finder')).toBe('bg-success');
});

test('a past week rows say what each result is, not just the week again', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Earlier week' }));
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  const recap = await card('Week 7 recap');
  expect(recap.getByText('League recap')).toBeInTheDocument();
  expect((await card('Week 7 retro')).getByText('Your lineup calls, looked back on')).toBeInTheDocument();
});

test('results read from the Sessions API carry no verdict: no tick, no failed-checks line, no minor notes', async () => {
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open Trends' }));
  await card('Trends');
  expect(screen.queryAllByRole('img', { name: 'Double-checked' })).toEqual([]);
  expect(screen.queryByText("Didn't pass its checks")).toBeNull();
  expect(document.body).not.toHaveTextContent(/\d+ minor notes?/);
});

test("Sleeper's live week is current even when only an earlier week has sessions", async () => {
  page(ALL.filter((r) => !r.key.includes(':9:')));
  expect(await screen.findByRole('heading', { name: 'Week 9' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Later week' })).toBeDisabled();
  expect(await screen.findByText('No lineup yet this week')).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: 'Run a job' }));
  expect(screen.getByRole('button', { name: 'Run Lineup' })).toBeEnabled();
});

test('a week with no sessions says there is nothing from it', async () => {
  page(ALL.filter((r) => !r.key.includes(':7:') && !r.key.includes(':9:')).filter((r) => !r.key.includes(':8:')).concat(row(6, 'digest')));
  await screen.findByRole('heading', { name: 'Week 9' });
  await userEvent.click(screen.getByRole('button', { name: 'Earlier week' }));
  expect(await screen.findByText('Nothing from this week.')).toBeInTheDocument();
});

test('trend prose keeps years but loses player ids', async () => {
  const t = JSON.parse(trends8.content);
  t.items[0].text = '4881 Lamar Jackson is out; the 2026 draft class has no 5000 yard rushers and Brees is headed to the 2026 Pro Bowl.';
  SEALED['8:trends'] = { ...trends8, content: JSON.stringify(t) };
  page();
  await userEvent.click(await screen.findByRole('button', { name: 'Open Trends' }));
  const card = await screen.findByRole('article', { name: 'Trends' });
  expect(card).toHaveTextContent('the 2026 draft class has no 5000 yard rushers and Brees is headed to the 2026 Pro Bowl');
  expect(card).not.toHaveTextContent('4881');
  SEALED['8:trends'] = trends8;
});

test('a run past the end of the week track is labelled Next, so it is not read as today', async () => {
  live = 8;
  // Now is Sun 10:00 AM ET; the track runs to next Sun midnight, so Sun 9:00 AM next week falls past it.
  RUNS.push({ job: 'history', at: '2026-11-08T14:00:00.000Z' });
  try {
    page(ALL.filter((r) => !r.key.endsWith(':9:lineup')));
    const strip = await screen.findByRole('region', { name: 'This week' });
    const row = (await within(strip).findByText('History')).closest('li')!;
    expect(row).toHaveTextContent(/Next Sun/);
    expect(within(strip).getByText('Trade finder').closest('li')).not.toHaveTextContent(/Next/);
  } finally {
    RUNS.pop();
  }
});

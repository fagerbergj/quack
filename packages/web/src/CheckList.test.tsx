import { act, cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import type { Check } from './api.ts';
import { CheckRow, Note, resolverLine } from './CheckList.tsx';
import { CheckProgress, CheckResult, STILL_MS } from './CheckResult.tsx';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const running = (over: object) => ({ criterion: 'c', id: 'sup', label: 'A claim is not stated', weight: 1, units: 60, ...over });

test('a running check shows how many items are done and a bar of settled by Clef, by the judge model, with it now, and waiting; the breakdown is its tooltip and label', () => {
  render(<CheckProgress checks={[running({ settled: { clef: 34, 'qwen3.8-flash-next': 6, code: 2 }, escalating: 4, waiting: 14 })]} />);
  const tip = '42 of 60 done, 34 by Clef, 6 by the judge model, 2 by code, 4 with the judge model now, 14 waiting';
  const row = screen.getByText(tip, { selector: '.sr-only' }).closest('li')!;
  expect(row).toHaveTextContent(/^A claim is not stated42\/60/);
  expect(row).toHaveAttribute('title', tip);
  expect(widths(row, 60)).toEqual([36, 6, 4]);
});

const widths = (row: HTMLElement, n: number) => [...row.querySelector('[data-bar]')!.children].map((s) => Math.round(parseFloat((s as HTMLElement).style.width) * n) / 100);

test('a re-check round credits answers reused from an earlier version to that version, not the judge model; undecided items close the sum', () => {
  render(<CheckProgress checks={[running({ units: 6, settled: { reused: 4, clef: 1 }, escalating: 0, waiting: 0 })]} />);
  const row = screen.getByText(/^6 of 6 done/, { selector: '.sr-only' }).closest('li')!;
  expect(row).toHaveAttribute('title', '6 of 6 done, 1 by Clef, 4 from earlier versions, 1 undecided or not needed');
  expect(widths(row, 6)).toEqual([6, 0, 0]);
  expect(resolverLine({ units: 6, resolvers: { reused: 4, 'qwen3.8-flash-next': 2 } })?.text).toBe('6 items · 2 by the judge model, 4 from earlier versions');
});

test('a running check that gains no answer for 2 minutes reads "Waiting…" until its count moves again', () => {
  vi.useFakeTimers();
  const { rerender } = render(<CheckProgress checks={[running({ settled: { clef: 10 }, escalating: 2, waiting: 48 })]} />);
  act(() => vi.advanceTimersByTime(STILL_MS - 15_000));
  expect(screen.getByText('10/60')).toBeInTheDocument();
  act(() => vi.advanceTimersByTime(15_000));
  expect(screen.getByText('Waiting…')).toBeInTheDocument();
  expect(screen.getByRole('listitem')).toHaveAttribute('title', expect.stringMatching(/^No new answer for 2 min\. 10 of 60 done/));
  expect(screen.getByText(/^No new answer for 2 min\./, { selector: '.sr-only' })).toBeInTheDocument();
  rerender(<CheckProgress checks={[running({ settled: { clef: 11 }, escalating: 2, waiting: 47 })]} />);
  expect(screen.getByText('11/60')).toBeInTheDocument();
});

test('a note prefixed with its check scope shows no unit chip', () => {
  render(<Note text="row: too long" scope="row" />);
  expect(screen.queryByText('row')).toBeNull();
  expect(screen.getByText('too long')).toBeTruthy();
});

test('an old-style sleeper note without a scope shows no chip for its whole-card unit', () => {
  render(<Note text="card: too long" />);
  expect(screen.queryByText('card')).toBeNull();
});

test('a row note keeps its unit chip', () => {
  render(<Note text="RB2: too long" scope="row" />);
  expect(screen.getByText('RB2')).toBeTruthy();
});

test('a failed check is marked ✗ with how many fell short; the count is on screen, the words in its tooltip and spoken name', () => {
  const base = { criterion: 'c', id: 'x', label: 'A finding cites no source', notes: [] };
  const checks = [{ ...base, weight: 3, met: 0.75, units: 4 }, { ...base, weight: -2, met: 0.75, units: 4 }, { ...base, weight: 3, met: 0.5, groups: 6, units: 20 }, { ...base, weight: 3, met: false, units: 2 }];
  render(<>{checks.map((c, i) => <CheckRow key={i} check={c} mark />)}</>);
  expect(screen.getAllByRole('img', { name: 'Failed' })).toHaveLength(4);
  const counts = screen.getAllByTitle(/fell short$/);
  expect(counts.map((p) => p.title)).toEqual(['1 of 4 items fell short', '3 of 4 items fell short', '3 of 6 groups fell short']);
  expect(counts[0]).toHaveTextContent(/^1 of 41 of 4 items fell short$/);
});

const check = (over: Partial<Check> = {}): Check => ({ criterion: 'claims_grounded', id: 'x', label: "A claim isn't stated by any source it cites", weight: 1, met: 1, notes: [], ...over });
const verdict = (checks: Check[]) => ({ passed: false, gatesFailed: [], score: 0.5, checks });

test('failing checks come first, open on their failing items with each note; a passed one is a closed ✓ row', () => {
  const failing = check({ id: 'sup', met: 0.95, units: 60, notes: ['f7: the source gives a lower rebate.', 'f9: the page lists no costs.'], resolvers: { clef: 52, 'qwen3.8-flash-next': 8 } });
  render(<CheckResult verdict={verdict([check({ id: 'ok', label: 'Cites a source that was not opened' }), failing])} open />);
  expect(screen.getAllByRole('img', { name: /Passed|Failed/ }).map((m) => m.getAttribute('aria-label'))).toEqual(['Failed', 'Passed']);
  const open = screen.getByText("A claim isn't stated by any source it cites").closest('details')!;
  expect(open).toHaveAttribute('open');
  expect(within(open).getByText('the source gives a lower rebate.')).toBeVisible();
  expect(within(open).getByText('the page lists no costs.')).toBeVisible();
  expect(within(open).getByText('3 of 60')).toBeInTheDocument();
});

test("a model check a failed gate kept from running is listed apart from the approve-time ones, as not run", () => {
  render(<CheckResult verdict={verdict([check({ id: 'g', weight: 'gate', met: 0 }), check({ id: 'm', label: 'A claim its sources don\'t back', met: null, deferred: 'gate' })])} open />);
  const group = screen.getByText('Run once the gates pass').parentElement!;
  expect(within(group).getByRole('img', { name: "Didn't run: a gate failed" })).toBeInTheDocument();
  expect(screen.queryByText('Run when you approve')).toBeNull();
});

test('a check opens on its first 5 failing items; the rest wait behind "N more failing"', async () => {
  const notes = Array.from({ length: 8 }, (_, i) => `f${i + 1}: reason ${i + 1}.`);
  render(<CheckResult verdict={verdict([check({ met: 0, units: 8, notes })])} open />);
  for (const n of [1, 5]) expect(screen.getByText(`reason ${n}.`)).toBeVisible();
  const more = screen.getByText('3 more failing');
  expect(more.closest('details')).not.toHaveAttribute('open');
  expect(screen.getByText('reason 6.')).not.toBeVisible();
  await userEvent.click(more);
  expect(screen.getByText('reason 8.')).toBeVisible();
});

test('an expanded check says how many items it ran over and who settled them, each model named in the tooltip', () => {
  render(<CheckResult verdict={verdict([check({ met: 0.9, units: 60, notes: ['f1: no.'], resolvers: { clef: 52, 'qwen3.8-flash-next': 8 } }), check({ id: 'one', units: 1, resolvers: { code: 1 } })])} open />);
  expect(screen.getByText('60 items · 52 by Clef, 8 by the judge model')).toHaveAttribute('title', '52 by clef, 8 by qwen3.8-flash-next');
  expect(screen.queryByText(/^1 items/)).toBeNull();
  expect(resolverLine({ units: 4, resolvers: { code: 1, clef: 1 }, reused: { a: 1, b: 1 } })?.text).toBe('4 items · 1 by Clef, 1 by code, 2 from earlier versions');
});

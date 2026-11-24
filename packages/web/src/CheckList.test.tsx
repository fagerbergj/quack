import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test } from 'vitest';
import type { Check } from './api.ts';
import { CheckRow, Note, resolverLine } from './CheckList.tsx';
import { CheckResult } from './CheckResult.tsx';

afterEach(cleanup);

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

test("a research report's failing item shows its claim, read from the report by finding and sentence, and the source it cites", () => {
  const report = ['## Summary', 'Heat pumps pay.', '', '## Findings', '- Rebates are 1500 per ton [s3]. They cap at 3 tons [s3].', '- Costs run 14k to 22k [s4].', '- Prices by size:', '  - A 2-ton unit is 9k [s1].'].join('\n');
  const notes = ['finding-1.2: source finding-1.2 ~ s3: The source gives a 2-ton cap.', 'finding-3.1: source finding-3.1 ~ s1: flagged by clef (no reason given): "A 2-ton unit is 9k"; re-verify it against the cited source before rewording it'];
  render(<CheckResult verdict={verdict([check({ met: 0.5, units: 4, groups: 4, notes })])} open about={{ kind: 'research.report', content: report }} />);
  expect(screen.getByText('finding-1.2 · s3')).toBeInTheDocument();
  expect(screen.getByText('“They cap at 3 tons.”')).toBeInTheDocument();
  expect(screen.getByText('→ s3')).toBeInTheDocument();
  expect(screen.getByText('The source gives a 2-ton cap.')).toBeInTheDocument();
  expect(screen.getByText('“A 2-ton unit is 9k.”')).toBeInTheDocument();
  expect(screen.getByText('Flagged by Clef, no reason given')).toHaveAttribute('title', expect.stringMatching(/^flagged by clef/));
});

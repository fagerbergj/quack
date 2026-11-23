import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { verdictMark, VersionMenu, type VersionItem } from './VersionMenu.tsx';

const STANDS = ['approved', 'changes asked', 'approved', 'changes asked', 'needs you'] as const;
const items: VersionItem[] = STANDS.map((status, i) => ({ id: i + 1, short: `v${i + 1}`, status, mark: status === 'approved' ? 'ok' : status === 'needs you' ? 'you' : 'bad' }));

test('the button and the menu show numbers and marks only; the standing is the spoken name and the tooltip', async () => {
  render(<VersionMenu name="Version" items={items} value={5} onPick={() => {}} />);
  const button = screen.getByRole('button', { name: 'Version: v5, needs you' });
  expect(button).toHaveTextContent(/^v5$/);
  expect(button).toHaveAttribute('title', 'v5, needs you');
  await userEvent.click(button);
  const menu = screen.getByRole('menu', { name: 'Version' });
  expect(menu).not.toHaveTextContent(/approved|changes|needs/);
  expect(within(menu).getByRole('menuitem', { name: 'v4, changes asked' })).toHaveAttribute('title', 'changes asked');
  expect(within(menu).queryByRole('menuitem', { name: 'Compare with…' }), 'no compare unless the caller offers it').toBeNull();
});

test('the menu lists the latest three, then Older…, which opens the rest in place and focuses the first of them', async () => {
  const onPick = vi.fn();
  render(<VersionMenu name="Version" items={items} value={5} onPick={onPick} />);
  await userEvent.click(screen.getByRole('button', { name: /^Version: / }));
  const menu = screen.getByRole('menu', { name: 'Version' });
  expect(within(menu).getAllByRole('menuitem').map((i) => i.textContent)).toEqual(['v5', 'v4', 'v3', 'Older…']);
  await userEvent.click(within(menu).getByRole('menuitem', { name: 'Older…' }));
  expect(within(menu).getAllByRole('menuitem').map((i) => i.textContent)).toEqual(['v5', 'v4', 'v3', 'v2', 'v1']);
  expect(document.activeElement).toHaveAccessibleName('v2, changes asked');
  await userEvent.click(within(menu).getByRole('menuitem', { name: 'v1, approved' }));
  expect(onPick).toHaveBeenCalledWith(1);
  expect(screen.queryByRole('menu')).toBeNull();
});

test('Compare with… swaps the menu for the versions older than the shown one, and picking one compares instead of switching', async () => {
  const onPick = vi.fn(), compare = vi.fn();
  render(<VersionMenu name="Version" items={items} value={3} compare={compare} onPick={onPick} />);
  await userEvent.click(screen.getByRole('button', { name: /^Version: / }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Compare with…' }));
  const menu = screen.getByRole('menu', { name: 'Compare v3 with' });
  expect(within(menu).getAllByRole('menuitem').map((i) => i.textContent)).toEqual(['v2', 'v1']);
  expect(document.activeElement).toHaveAccessibleName('v2, changes asked');
  await userEvent.click(within(menu).getByRole('menuitem', { name: 'v1, approved' }));
  expect([compare.mock.calls, onPick.mock.calls]).toEqual([[[1]], []]);
});

test('the oldest version offers nothing to compare with', async () => {
  render(<VersionMenu name="Version" items={items} value={1} compare={() => {}} onPick={() => {}} />);
  await userEvent.click(screen.getByRole('button', { name: /^Version: / }));
  expect(screen.queryByRole('menuitem', { name: 'Compare with…' })).toBeNull();
});

test("a verdict's mark and words: checking, passed, and failed with the count to fix", () => {
  const check = { criterion: 'c', id: 'c', label: 'c', weight: 1, met: 0, notes: [] };
  expect([verdictMark(undefined), verdictMark(null)]).toEqual([{}, { mark: 'checking', status: 'checking' }]);
  expect(verdictMark({ passed: true, gatesFailed: [], score: 1, checks: [] })).toEqual({ mark: 'ok', status: 'passed checks' });
  expect(verdictMark({ passed: false, gatesFailed: [], score: 0, checks: [check, check] })).toEqual({ mark: 'bad', status: 'failed checks, 2 to fix' });
});

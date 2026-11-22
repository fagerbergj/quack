import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import { RailBody } from './Rail.tsx';
import { needId } from './sessions.ts';

afterEach(() => (vi.unstubAllGlobals(), document.querySelector('section[id]')?.remove()));

async function jumpTo(reduce: boolean) {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: reduce && q.includes('prefers-reduced-motion'), addEventListener() {}, removeEventListener() {} }));
  const target = document.body.appendChild(document.createElement('section'));
  target.id = needId('q1');
  const scroll = (target.scrollIntoView = vi.fn());
  const q = { id: 'q1', question: 'Ship it?', options: [], evidence: null, askedAt: 1 };
  render(<RailBody sessionKey="ui:u1" title="t" view={{ items: [] } as never} meta={[]} items={[]} approvals={[]} questions={[q]} />);
  await userEvent.click(screen.getByRole('button', { name: 'Ship it?' }));
  return scroll;
}

test('a jump from the rail scrolls smoothly', async () => {
  expect(await jumpTo(false)).toHaveBeenCalledWith({ behavior: 'smooth', block: 'start' });
});

test('a jump from the rail does not animate for a person who asked for reduced motion', async () => {
  expect(await jumpTo(true)).toHaveBeenCalledWith({ behavior: 'auto', block: 'start' });
});

const sealed = (n: number) => ({ kind: 'review', version: n, sealedAt: n * 1000, content: '', summary: '' }) as never;
const versionsOf = (n: number) => render(<RailBody sessionKey="ui:u1" title="t" view={{ items: [] } as never} meta={[]} items={Array.from({ length: n }, (_, i) => sealed(i + 1))} approvals={[]} questions={[]} />);

test('a long version history shows the latest three, newest first, and the rest on request', async () => {
  versionsOf(8);
  const list = () => screen.getAllByRole('button', { name: /^v\d/ }).map((b) => b.textContent);
  expect(list()).toEqual(['v8', 'v7', 'v6']);
  const more = screen.getByRole('button', { name: 'Older…' });
  expect(more).toHaveAttribute('aria-expanded', 'false');
  await userEvent.click(more);
  expect(list()).toHaveLength(8);
  expect(screen.getByRole('button', { name: 'Fewer' })).toHaveAttribute('aria-expanded', 'true');
});

test('a fourth version earns the disclosure, which collapses again', async () => {
  versionsOf(4);
  const more = screen.getByRole('button', { name: 'Older…' });
  expect(more).toHaveAttribute('aria-controls', 'rail-version-list');
  await userEvent.click(more);
  expect(screen.getAllByRole('button', { name: /^v\d/ })).toHaveLength(4);
  await userEvent.click(screen.getByRole('button', { name: 'Fewer' }));
  expect(screen.getAllByRole('button', { name: /^v\d/ })).toHaveLength(3);
  expect(screen.getByRole('button', { name: 'Older…' })).toHaveAttribute('aria-expanded', 'false');
});

test('a version sealed again counts once, by its newest seal', () => {
  const again = { kind: 'review', version: 2, sealedAt: 2500, content: '', summary: '' } as never;
  render(<RailBody sessionKey="ui:u1" title="t" view={{ items: [] } as never} meta={[]} items={[sealed(1), sealed(2), again, sealed(3), sealed(4)]} approvals={[]} questions={[]} />);
  expect(screen.getAllByRole('button', { name: /^v\d/ })).toHaveLength(3);
  expect(screen.getByRole('button', { name: 'Older…' })).toBeInTheDocument();
});

test('a row shows only its number and mark; what the mark means, the count to fix and the date are its tooltip and label', () => {
  const check = { criterion: 'c', id: 'c', label: 'c', weight: -3, met: 1, notes: [] };
  const failed = { kind: 'review', version: 1, sealedAt: 1000, content: '', summary: '', verdict: { passed: false, gatesFailed: [], score: 0.5, checks: [check, check] } } as never;
  render(<RailBody sessionKey="ui:u1" title="t" view={{ items: [] } as never} meta={[]} items={[failed]} approvals={[]} questions={[]} />);
  const row = screen.getByRole('button', { name: /v1/ });
  expect(row).toHaveAccessibleName(/^v1, failed checks, 2 to fix, /);
  expect(row).toHaveAttribute('title', row.getAttribute('aria-label'));
  expect(row).toHaveTextContent(/^v1$/);
});

test('three versions or fewer have no disclosure', () => {
  versionsOf(3);
  expect(screen.getAllByRole('button', { name: /^v\d/ })).toHaveLength(3);
  expect(screen.queryByRole('button', { name: /older|fewer/i })).toBeNull();
});

test("a version whose extension gives a standing (a contract's decision) is marked by it, not by its verdict", () => {
  const passed = { kind: 'contract.spec', version: 1, sealedAt: 1000, content: '', summary: '', verdict: { passed: true, gatesFailed: [], score: 1, checks: [] } } as never;
  render(<RailBody sessionKey="contracts:c1" title="t" view={{ items: [] } as never} meta={[]} items={[passed]} approvals={[]} questions={[]} markOf={() => ({ mark: 'bad', status: 'changes asked' })} />);
  const row = screen.getByRole('button', { name: /^v1, changes asked, / });
  expect(row.querySelector('[data-mark]')).toHaveAttribute('data-mark', 'bad');
});

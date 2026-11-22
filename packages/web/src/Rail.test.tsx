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
  const list = () => screen.getAllByRole('button', { name: /v\d/ }).map((b) => /v\d+/.exec(b.textContent ?? '')![0]);
  expect(list()).toEqual(['v8', 'v7', 'v6']);
  const more = screen.getByRole('button', { name: '5 older versions' });
  expect(more).toHaveAttribute('aria-expanded', 'false');
  await userEvent.click(more);
  expect(list()).toHaveLength(8);
  expect(screen.getByRole('button', { name: 'Show fewer versions' })).toHaveAttribute('aria-expanded', 'true');
});

test('a fourth version earns a singular disclosure that collapses again', async () => {
  versionsOf(4);
  const more = screen.getByRole('button', { name: '1 older version' });
  expect(more).toHaveAttribute('aria-controls', 'rail-version-list');
  await userEvent.click(more);
  expect(screen.getAllByRole('button', { name: /v\d/ })).toHaveLength(4);
  await userEvent.click(screen.getByRole('button', { name: 'Show fewer versions' }));
  expect(screen.getAllByRole('button', { name: /v\d/ })).toHaveLength(3);
  expect(screen.getByRole('button', { name: '1 older version' })).toHaveAttribute('aria-expanded', 'false');
});

test('a version sealed again counts once, by its newest seal', () => {
  const again = { kind: 'review', version: 2, sealedAt: 2500, content: '', summary: '' } as never;
  render(<RailBody sessionKey="ui:u1" title="t" view={{ items: [] } as never} meta={[]} items={[sealed(1), sealed(2), again, sealed(3), sealed(4)]} approvals={[]} questions={[]} />);
  expect(screen.getAllByRole('button', { name: /v\d/ })).toHaveLength(3);
  expect(screen.getByRole('button', { name: '1 older version' })).toBeInTheDocument();
});

test('a failed version shows no score; its tooltip and label carry the count to fix', () => {
  const check = { criterion: 'c', id: 'c', label: 'c', weight: -3, met: 1, notes: [] };
  const failed = { kind: 'review', version: 1, sealedAt: 1000, content: '', summary: '', verdict: { passed: false, gatesFailed: [], score: 0.5, checks: [check, check] } } as never;
  render(<RailBody sessionKey="ui:u1" title="t" view={{ items: [] } as never} meta={[]} items={[failed]} approvals={[]} questions={[]} />);
  const row = screen.getByRole('button', { name: /v1/ });
  expect(row).toHaveAccessibleName(/Didn't pass its checks · 2 to fix/);
  expect(row).toHaveAttribute('title', expect.stringContaining('2 to fix'));
  expect(row).not.toHaveTextContent(/%|to fix/);
});

test('three versions or fewer have no disclosure', () => {
  versionsOf(3);
  expect(screen.getAllByRole('button', { name: /v\d/ })).toHaveLength(3);
  expect(screen.queryByRole('button', { name: /older|fewer/ })).toBeNull();
});

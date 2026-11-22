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

test('three versions or fewer have no disclosure', () => {
  versionsOf(3);
  expect(screen.getAllByRole('button', { name: /v\d/ })).toHaveLength(3);
  expect(screen.queryByRole('button', { name: /older|fewer/ })).toBeNull();
});

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

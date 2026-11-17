import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import { ClampedTitle } from './RestView.tsx';

// jsdom lays nothing out: the measurement is scrollHeight against clientHeight, so a test sets the pair.
function measure(scroll: number, client: number) {
  vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockReturnValue(scroll);
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(client);
}
afterEach(() => vi.restoreAllMocks());

test('a title that fits stays plain text, not a control', () => {
  measure(44, 44);
  render(<ClampedTitle text="Short" />);
  expect(screen.queryByRole('button')).toBeNull();
  expect(screen.getByText('Short')).toHaveClass('max-sm:line-clamp-2');
});

test('a clamped title is a collapsed control that opens in place on a tap and on Enter or Space, and closes again', async () => {
  measure(80, 44);
  render(<ClampedTitle text="A very long contract name" />);
  const b = screen.getByRole('button', { name: 'A very long contract name' });
  expect(b).toHaveAttribute('aria-expanded', 'false');
  expect(screen.getByText('A very long contract name')).toHaveClass('max-sm:line-clamp-2');
  await userEvent.click(b);
  expect(b).toHaveAttribute('aria-expanded', 'true');
  expect(screen.getByText('A very long contract name')).not.toHaveClass('max-sm:line-clamp-2');
  b.focus();
  await userEvent.keyboard('{Enter}');
  expect(b).toHaveAttribute('aria-expanded', 'false');
  await userEvent.keyboard(' ');
  expect(b).toHaveAttribute('aria-expanded', 'true');
  await userEvent.click(b);
  expect(screen.getByRole('button')).toHaveAttribute('aria-expanded', 'false');
});

import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { Shell } from './Shell.tsx';
import { useHarness } from './test-harness.ts';

useHarness(() => ({}));

const more = () => screen.getByText('More', { selector: 'summary' }).closest('details')!;

test('More opens when navigating to a rest route and can still be collapsed', async () => {
  const { rerender } = render(<Shell route="home"><div /></Shell>);
  expect(more().open).toBe(false);

  rerender(<Shell route="settings"><div /></Shell>);
  expect(more().open).toBe(true);

  await userEvent.click(more().querySelector('summary')!);
  expect(more().open).toBe(false);

  // Re-rendering on the same rest route keeps the user's collapsed choice.
  rerender(<Shell route="settings"><div /></Shell>);
  expect(more().open).toBe(false);

  // Leaving and returning to a rest route opens it again.
  rerender(<Shell route="home"><div /></Shell>);
  expect(more().open).toBe(false);
  rerender(<Shell route="settings"><div /></Shell>);
  expect(more().open).toBe(true);
});

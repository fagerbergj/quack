import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import { DiffContext } from './diff.ts';
import { SchemaTree } from './SchemaTree.tsx';
import type { Field } from './spec.ts';

const f = (name: string, depth: number): Field => ({ name, type: 'any', required: true, depth, ptr: `/${name}`, facts: [] });

test('a nested field says how deep it sits and carries a guide line per ancestor', () => {
  render(<SchemaTree rows={[f('sections', 0), f('findings', 1), f('first', 2)]} whyAt={() => []} />);
  const rows = screen.getAllByRole('listitem');
  expect(rows.map((r) => r.getAttribute('aria-level'))).toEqual(['1', '2', '3']);
  expect(rows.map((r) => r.querySelectorAll('[data-guide]').length)).toEqual([0, 1, 2]);
});

const withMarks = (ptrs: string[], rows: Field[]) =>
  render(<DiffContext.Provider value={new Map(ptrs.map((p) => [p, 'added' as const]))}><SchemaTree rows={rows} whyAt={() => []} /></DiffContext.Provider>);

test('a new object gets one named marker and its children get none', () => {
  withMarks(['/waiting', '/waiting/since'], [f('note', 0), f('waiting', 0), { ...f('since', 1), ptr: '/waiting/since' }]);
  expect(screen.getAllByRole('img', { name: 'New' })).toHaveLength(1);
  expect(screen.queryByText('NEW')).toBeNull();
});

test('a changed field in an unchanged parent is marked, in the same gutter as a top-level one', () => {
  const { container } = withMarks(['/a', '/b'], [f('wake', 0), f('a', 1), f('b', 0)]);
  expect(screen.getAllByRole('img', { name: 'New' })).toHaveLength(2);
  const gutters = [...container.querySelectorAll<HTMLElement>('[data-gutter]')];
  expect(new Set(gutters.map((g) => g.className)).size).toBe(1);
  // Each dot's offset cancels its row's indent, so both land at x = 0.
  const x = (g: HTMLElement) => parseFloat(g.style.left) + parseFloat(g.closest('li')!.style.paddingLeft);
  expect(gutters.map(x)).toEqual([0, 0]);
});

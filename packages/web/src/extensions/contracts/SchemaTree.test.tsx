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
  expect(screen.queryByText('New')).toBeNull();
});

test('a changed field in an unchanged parent is marked, in a gutter left of the guides at any depth', () => {
  const { container } = withMarks(['/a', '/b'], [f('wake', 0), f('a', 1), f('b', 0)]);
  expect(screen.getAllByRole('img', { name: 'New' })).toHaveLength(2);
  const gutters = [...container.querySelectorAll<HTMLElement>('[data-gutter]')];
  const row = (g: HTMLElement) => g.closest('li')!;
  // A dot's left is relative to the name, which starts at the row's padding.
  expect(gutters.map((g) => parseFloat(g.style.left) + parseFloat(row(g).style.paddingLeft))).toEqual([0, 0]);
  const guide = row(gutters[0]).querySelector<HTMLElement>('[data-guide]')!;
  expect(14, 'the gutter ends before the first guide').toBeLessThan(parseFloat(guide.style.left));
});

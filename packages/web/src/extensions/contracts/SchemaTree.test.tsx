import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import { SchemaTree } from './SchemaTree.tsx';
import type { Field } from './spec.ts';

const f = (name: string, depth: number): Field => ({ name, type: 'any', required: true, depth, ptr: `/${name}`, facts: [] });

test('a nested field says how deep it sits and carries a guide line per ancestor', () => {
  render(<SchemaTree rows={[f('sections', 0), f('findings', 1), f('first', 2)]} whyAt={() => []} />);
  const rows = screen.getAllByRole('listitem');
  expect(rows.map((r) => r.getAttribute('aria-level'))).toEqual(['1', '2', '3']);
  expect(rows.map((r) => r.querySelectorAll('span[aria-hidden]:empty').length)).toEqual([0, 1, 2]);
});

import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import { Content } from './JsonView.tsx';

const doc = { summary: 'Hello', url: 'https://example.com/a', n: 3, ok: true, none: null, items: [{ name: 'Alpha', deep: { a: { b: { c: 'hidden' } } } }, { name: 'Beta' }] };

test('JSON text renders as a tree with links, counts and item names', () => {
  render(<Content text={JSON.stringify(doc)} />);
  expect(screen.getByRole('link', { name: 'https://example.com/a' })).toHaveAttribute('href', 'https://example.com/a');
  expect(screen.getByText('2 items')).toBeInTheDocument();
  expect(screen.getAllByText('Alpha')).toHaveLength(2);
  expect(screen.getByText('null')).toBeInTheDocument();
});

test('top levels are open and deep ones folded', () => {
  render(<Content text={JSON.stringify(doc)} />);
  const open = (name: string) => (screen.getByText(name, { selector: 'span' }).closest('details') as HTMLDetailsElement).open;
  expect(open('items')).toBe(true);
  expect(open('deep')).toBe(false);
});

test('plain text still renders as markdown', () => {
  render(<Content text={'# Title\n\nbody'} />);
  expect(screen.getByRole('heading', { name: 'Title' })).toBeInTheDocument();
});

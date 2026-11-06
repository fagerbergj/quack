import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, test } from 'vitest';
import { Note } from './CheckList.tsx';

afterEach(cleanup);

test('a note prefixed with its check scope shows no unit chip', () => {
  render(<Note text="row: too long" scope="row" />);
  expect(screen.queryByText('row')).toBeNull();
  expect(screen.getByText('too long')).toBeTruthy();
});

test('an old-style sleeper note without a scope shows no chip for its whole-card unit', () => {
  render(<Note text="card: too long" />);
  expect(screen.queryByText('card')).toBeNull();
});

test('a row note keeps its unit chip', () => {
  render(<Note text="RB2: too long" scope="row" />);
  expect(screen.getByText('RB2')).toBeTruthy();
});

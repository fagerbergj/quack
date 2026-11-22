import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, test } from 'vitest';
import { Note, Pill } from './CheckList.tsx';

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

test('a failed check says Fail and keeps how many fell short in its tooltip and its spoken name', () => {
  const base = { criterion: 'c', id: 'x', label: 'A finding cites no source', notes: [] };
  render(<>
    <Pill check={{ ...base, weight: 3, met: 0.75, units: 4 }} />
    <Pill check={{ ...base, weight: -2, met: 0.75, units: 4 }} />
    <Pill check={{ ...base, weight: 3, met: 0.5, groups: 6, units: 20 }} />
    <Pill check={{ ...base, weight: 3, met: false, units: 2 }} />
    <Pill check={{ ...base, weight: 3, met: 0.6 }} />
  </>);
  const pills = screen.getAllByText('Fail');
  expect(pills.map((p) => p.title)).toEqual(['1 of 4 items fell short', '3 of 4 items fell short', '3 of 6 groups fell short', '', '']);
  expect(pills[0]).toHaveTextContent('Fail, 1 of 4 items fell short');
});

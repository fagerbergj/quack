import { expect, test } from 'vitest';
import { wordDiff } from './lineDiff.ts';

const show = (a: string, b: string) => wordDiff(a, b).map((p) => `${p.kind}:${p.text}`);

test('wordDiff marks inserted, deleted and replaced words, a replaced phrase as one struck run then one added run', () => {
  expect(show('a key that 404s', 'a key that always 404s')).toEqual(['same:a key that ', 'add:always ', 'same:404s']);
  expect(show('a key that always 404s', 'a key that 404s')).toEqual(['same:a key that ', 'del:always ', 'same:404s']);
  expect(show('runs soonest first', 'runs from the start, oldest first')).toEqual(['same:runs ', 'del:soonest ', 'add:from the start, oldest ', 'same:first']);
});

test('wordDiff folds a long unchanged run into a gap that holds its words, keeping six words beside each change', () => {
  const words = Array.from({ length: 20 }, (_, i) => `w${i}`);
  const parts = wordDiff(['x', ...words, 'y'].join(' '), ['X', ...words, 'Y'].join(' '));
  expect(parts.map((p) => p.kind)).toEqual(['del', 'add', 'same', 'gap', 'same', 'del', 'add']);
  expect(parts[2]!.text).toBe(' w0 w1 w2 w3 w4 w5 ');
  expect(parts[3]!.text).toBe(words.slice(6, 14).join(' ') + ' ');
  expect(parts[4]!.text).toBe('w14 w15 w16 w17 w18 w19 ');
  expect(wordDiff(`${words.join(' ')} end`, `${words.join(' ')} stop`)[0]).toEqual({ kind: 'gap', text: `${words.slice(0, 14).join(' ')} ` });
});

test('wordDiff of an empty side is one whole insert or delete, and equal texts have no diff', () => {
  expect(show('', 'a new field')).toEqual(['add:a new field']);
  expect(show('an old field', '')).toEqual(['del:an old field']);
  expect(wordDiff('', '')).toEqual([]);
  expect(wordDiff('same', 'same')).toEqual([]);
});

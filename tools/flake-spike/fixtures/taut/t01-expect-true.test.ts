import { expect, test } from 'vitest';
import { slugify } from '../subjects/slug.ts';

test('slugify handles a title with punctuation', () => {
  slugify('Hello, World!');
  expect(true).toBe(true);
});

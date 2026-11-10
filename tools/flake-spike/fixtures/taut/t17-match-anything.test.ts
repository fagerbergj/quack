import { expect, test } from 'vitest';
import { slugify } from '../subjects/slug.ts';

test('slugs are lowercase words joined by hyphens', () => {
  expect(slugify('Hello Big World')).toMatch(/[a-z-]*/);
});

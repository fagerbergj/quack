import { expect, test } from 'vitest';
import { slugify } from '../subjects/slug.ts';

test.each([
  ['Hello World', 'hello-world'],
  ['  The Quick  Brown Fox  ', 'the-quick-brown-fox'],
  ['C++ & Rust!', 'c-rust'],
  ['---', ''],
  ['v2.0 Release', 'v2-0-release'],
])('slugify(%j) is %j', (title, slug) => {
  expect(slugify(title)).toBe(slug);
});

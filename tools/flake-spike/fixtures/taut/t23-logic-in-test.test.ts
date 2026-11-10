import assert from 'node:assert/strict';
import { test } from 'node:test';
import { slugify } from '../subjects/slug.ts';

test('slugs for assorted titles', () => {
  for (const title of ['Hello World', 'C++ & Rust', '  spaced  out  ', 'Ünïcode']) {
    let expected = title.toLowerCase().trim().replace(/[^a-z0-9]+/g, '-');
    if (expected.startsWith('-')) expected = expected.slice(1);
    if (expected.endsWith('-')) expected = expected.slice(0, -1);
    assert.equal(slugify(title), expected.replace(/^-+|-+$/g, ''));
  }
});

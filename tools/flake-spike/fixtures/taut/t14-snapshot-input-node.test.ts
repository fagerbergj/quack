import assert from 'node:assert/strict';
import { test } from 'node:test';
import { slugify } from '../subjects/slug.ts';

test('slugs for a batch of titles', () => {
  const titles = ['Hello World', 'A -- B', '  trailing  '];
  const slugs = titles.map(slugify);
  assert.deepEqual(titles, ['Hello World', 'A -- B', '  trailing  ']);
  assert.equal(slugs.length, titles.length);
});

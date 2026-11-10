import assert from 'node:assert/strict';
import { test } from 'node:test';
import { slugify } from '../subjects/slug.ts';

test('a title becomes a slug', () => {
  const slug = slugify('  The Quick Brown Fox  ');
  assert.ok(slug);
});

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { slugify } from '../subjects/slug.ts';

test('slugify returns a slug', () => {
  assert.equal(typeof slugify('Release Notes 2.0'), 'string');
});

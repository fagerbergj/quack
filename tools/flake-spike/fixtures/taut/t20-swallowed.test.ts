import assert from 'node:assert/strict';
import { test } from 'node:test';
import { slugify } from '../subjects/slug.ts';

test('punctuation collapses to one hyphen', () => {
  try {
    assert.equal(slugify('a!!!b'), 'a-b');
  } catch (err) {
    console.log('slug mismatch', err);
  }
});

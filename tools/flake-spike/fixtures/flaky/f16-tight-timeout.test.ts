import assert from 'node:assert/strict';
import { pbkdf2Sync } from 'node:crypto';
import { test } from 'node:test';
import { slugify } from '../subjects/slug.ts';

test('slugs of hashed titles stay short', { timeout: 400 }, async () => {
  const titles = Array.from({ length: 12 }, (_, i) => pbkdf2Sync(`t${i}`, 'salt', 30_000, 16, 'sha256').toString('hex'));
  await new Promise((r) => setImmediate(r));
  for (const t of titles) assert.ok(slugify(`Post ${t}`).length <= 40);
});

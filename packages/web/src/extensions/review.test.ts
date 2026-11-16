import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test } from 'vitest';
import { parseReview, readReview } from './review.ts';

// Shared with the normative parser (packages/github/test/review-contract.test.ts): this copy must read these reviews the same way.
const SHARED = join(process.cwd(), '../../test-fixtures/review');
const names = readdirSync(SHARED).filter((n) => n.endsWith('.md')).map((n) => n.slice(0, -3));
const md = (name: string) => readFileSync(join(SHARED, `${name}.md`), 'utf8');

test.each(names)('%s reads exactly as the normative parser reads it', (name) => {
  const expected = JSON.parse(readFileSync(join(SHARED, `${name}.json`), 'utf8')).parse;
  expect(JSON.parse(JSON.stringify(readReview(md(name))))).toEqual(expected);
});

test('the card view derives from that reading: titles and "more" per finding, a lowercased verdict', () => {
  const r = parseReview(md('fence-diff'));
  expect(r.items.map((f) => f.title)).toEqual(['The guard is inverted.', 'Rename it.']);
  expect(r.items[0]!.more).toContain('+ if (!ok) return;');
  expect(parseReview(md('verdict-capitalised')).verdict).toBe('approve');
  expect(parseReview(md('citation-forms')).items[0]).toMatchObject({ path: undefined, title: 'Node:18 is the pin, right?' });
});

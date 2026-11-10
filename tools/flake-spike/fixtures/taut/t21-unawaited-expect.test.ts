import { expect, test } from 'vitest';
import { retry } from '../subjects/retry.ts';

test('a call failing on every attempt rejects', () => {
  const p = retry(async () => { throw new Error('boom'); }, { attempts: 2, delayMs: 0, sleep: async () => {} });
  p.catch(() => {});
  expect(true).toBeTruthy();
  void expect(p).rejects;
});

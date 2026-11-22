import { expect, test } from 'vitest';
import { age, spendLabel } from './sessions.ts';

const NOW = Date.parse('2026-11-10T12:00:00.000Z');
const ago = (ms: number) => new Date(NOW - ms).toISOString();

test('age reads now, minutes, hours, then a calendar day', () => {
  expect(age(ago(30_000), NOW)).toBe('now');
  expect(age(ago(5 * 60_000), NOW)).toBe('5m');
  expect(age(ago(3 * 3_600_000), NOW)).toBe('3h');
  expect(age('2026-10-01T12:00:00.000Z', NOW)).toMatch(/^(Oct 1|1 Oct)$/);
});

const spend = (cost: number, tokens: object) => ({ cost, tokens: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, ...tokens } });

test('spendLabel keeps its text and token breakdown', () => {
  expect(spendLabel(undefined)).toBeUndefined();
  expect(spendLabel(spend(0, {}))).toBeUndefined();
  expect(spendLabel(spend(0.004, { input: 1 }))?.text).toBe('<$0.01');
  const l = spendLabel(spend(1.234, { input: 1234567, output: 89, cacheRead: 1000, cacheWrite: 2 }))!;
  expect(l.text).toBe('$1.23');
  expect(l.title.replace(/[,.   ]/g, '')).toBe('1234567in·89out·1000cached·2cachewrite');
  expect(spendLabel(spend(0, { input: 2000000, output: 417000 }))?.text).toBe('2.4M tokens');
});

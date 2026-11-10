import { afterEach, expect, test, vi } from 'vitest';
import { debounce } from '../subjects/debounce.ts';

afterEach(() => vi.useRealTimers());

test('a burst of saves writes once, with the last value, after the quiet period', () => {
  vi.useFakeTimers();
  const write = vi.fn();
  const save = debounce(write, 30);
  save(1);
  vi.advanceTimersByTime(20);
  save(2);
  save(3);
  vi.advanceTimersByTime(29);
  expect(write).not.toHaveBeenCalled();
  vi.advanceTimersByTime(1);
  expect(write.mock.calls).toEqual([[3]]);
});

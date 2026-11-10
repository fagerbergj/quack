import { readFile } from 'node:fs/promises';
import { expect, test, vi } from 'vitest';
import { debounce } from '../subjects/debounce.ts';

test('a burst of saves writes once', async () => {
  const write = vi.fn();
  const save = debounce(write, 30);
  void readFile(new URL(import.meta.url)).then(() => { save(1); save(2); save(3); });
  await new Promise((r) => setTimeout(r, 40));
  expect(write).toHaveBeenCalledTimes(1);
  expect(write).toHaveBeenCalledWith(3);
});

import { act, renderHook } from '@testing-library/react';
import { expect, test } from 'vitest';
import { useLoad } from './useLoad.ts';

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: Error) => void;
  const promise = new Promise<T>((res, rej) => ((resolve = res), (reject = rej)));
  return { promise, resolve, reject };
}

test('an answer to an older load never replaces the newer one', async () => {
  const loads = [deferred<string>(), deferred<string>()];
  const { result, rerender } = renderHook(({ n }) => useLoad(() => loads[n]!.promise, [n], ''), { initialProps: { n: 0 } });
  rerender({ n: 1 });
  await act(async () => loads[1]!.resolve('new'));
  await act(async () => loads[0]!.resolve('old'));
  expect(result.current).toMatchObject({ data: 'new', error: '' });
});

test('a later success clears an earlier error and keeps the last good data meanwhile', async () => {
  const loads = [deferred<string>(), deferred<string>(), deferred<string>()];
  const { result, rerender } = renderHook(({ n }) => useLoad(() => loads[n]!.promise, [n], ''), { initialProps: { n: 0 } });
  await act(async () => loads[0]!.resolve('first'));
  rerender({ n: 1 });
  await act(async () => loads[1]!.reject(new Error('down')));
  expect(result.current).toMatchObject({ data: 'first', error: 'down' });
  rerender({ n: 2 });
  await act(async () => loads[2]!.resolve('back'));
  expect(result.current).toMatchObject({ data: 'back', error: '' });
});

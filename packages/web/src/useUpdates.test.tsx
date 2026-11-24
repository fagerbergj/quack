import { act, render, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { outdated, unsaved, useUpdates } from './useUpdates.ts';
import type { Version } from './api.ts';
import { useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /version': { commit: 'old', builtAt: null } }));

let state: DocumentVisibilityState = 'visible';
const turn = async (to: DocumentVisibilityState) => {
  state = to;
  await act(async () => void document.dispatchEvent(new Event('visibilitychange')));
};

beforeEach(() => {
  state = 'visible';
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state);
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// The build this page loaded with is read on mount; a deploy's restart reopens the change stream, whose hello names the new build.
const mount = async () => {
  const reload = vi.fn();
  const hook = renderHook(({ served }) => useUpdates(served, reload), { initialProps: { served: { commit: 'old', builtAt: null } as Version | undefined } });
  await act(async () => {});
  return { reload, hello: async (commit: string) => act(async () => hook.rerender({ served: { commit, builtAt: null } })) };
};

test('a build is outdated only when both sides are known and differ', () => {
  expect(outdated('a', 'b')).toBe(true);
  expect(outdated('a', 'a')).toBe(false);
  expect(outdated(null, 'b')).toBe(false);
  expect(outdated('a', undefined)).toBe(false);
});

test('a typed field, pending attachments or an open dialog count as unsaved; an empty form does not', () => {
  const { rerender } = render(<form><textarea defaultValue="" /><input type="checkbox" defaultChecked /></form>);
  expect(unsaved()).toBe(false);
  rerender(<form><textarea defaultValue="half a message" /></form>);
  expect(unsaved()).toBe(true);
  rerender(<ul data-unsaved />);
  expect(unsaved()).toBe(true);
  rerender(<section role="dialog" />);
  expect(unsaved()).toBe(true);
});

test('coming back to a clean page after a deploy reloads it at once', async () => {
  const { reload, hello } = await mount();
  await turn('hidden');
  await hello('new');
  expect(reload).not.toHaveBeenCalled();
  await turn('visible');
  expect(reload).toHaveBeenCalledOnce();
});

test('a dirty draft defers the reload to the next time the page is hidden and clean', async () => {
  const draft = document.body.appendChild(Object.assign(document.createElement('textarea'), { value: 'not sent yet' }));
  const { reload, hello } = await mount();
  await hello('new');
  await turn('visible');
  expect(reload).not.toHaveBeenCalled();
  await turn('hidden');
  expect(reload).not.toHaveBeenCalled();
  draft.value = '';
  await turn('hidden');
  expect(reload).toHaveBeenCalledOnce();
  draft.remove();
});

test('a visible clean page reloads as the hello names a new build, and never for the same build', async () => {
  const { reload, hello } = await mount();
  await hello('old');
  await act(async () => vi.advanceTimersByTimeAsync(60 * 60_000));
  expect(reload, 'nothing is polled').not.toHaveBeenCalled();
  await hello('new');
  expect(reload).toHaveBeenCalledOnce();
});

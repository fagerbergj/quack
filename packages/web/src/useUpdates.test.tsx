import { act, render, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { outdated, unsaved, useUpdates } from './useUpdates.ts';
import { routes, useHarness } from './test-harness.ts';

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

// The build this page loaded with is read on mount; a deploy changes what /version answers afterwards.
const mount = async () => {
  const reload = vi.fn();
  renderHook(() => useUpdates(reload));
  await act(async () => {});
  routes['GET /version'] = { commit: 'new', builtAt: null };
  return reload;
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
  const reload = await mount();
  await turn('hidden');
  expect(reload).not.toHaveBeenCalled();
  await turn('visible');
  expect(reload).toHaveBeenCalledOnce();
});

test('a dirty draft defers the reload to the next time the page is hidden and clean', async () => {
  const draft = document.body.appendChild(Object.assign(document.createElement('textarea'), { value: 'not sent yet' }));
  const reload = await mount();
  await turn('visible');
  expect(reload).not.toHaveBeenCalled();
  await turn('hidden');
  expect(reload).not.toHaveBeenCalled();
  draft.value = '';
  await turn('hidden');
  expect(reload).toHaveBeenCalledOnce();
  draft.remove();
});

test('a visible page checks every five minutes, and an unchanged build never reloads', async () => {
  const reload = vi.fn();
  renderHook(() => useUpdates(reload));
  await act(async () => vi.advanceTimersByTimeAsync(5 * 60_000));
  expect(reload).not.toHaveBeenCalled();
  routes['GET /version'] = { commit: 'new', builtAt: null };
  await act(async () => vi.advanceTimersByTimeAsync(5 * 60_000));
  expect(reload).toHaveBeenCalledOnce();
});

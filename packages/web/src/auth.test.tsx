import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { apiFetch, navigation, signIn, SignInExpired, signInStuck } from './auth.ts';
import { SignInBanner } from './SignInBanner.tsx';

beforeEach(() => {
  sessionStorage.clear();
  signInStuck.forget();
  vi.spyOn(navigation, 'reload').mockImplementation(() => {});
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(Date.parse('2026-11-25T12:00:00Z'));
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

test('a call redirected to another origin sends the page to sign in and throws SignInExpired', async () => {
  // Signed out, the proxy redirects to its login origin: opaque under manual redirect, a CORS failure when followed.
  vi.stubGlobal('fetch', async (_url: string, init?: RequestInit) => {
    if (init?.redirect === 'manual') return { type: 'opaqueredirect', ok: false, status: 0 } as Response;
    throw new TypeError('Failed to fetch');
  });
  await expect(apiFetch('sessions')).rejects.toBeInstanceOf(SignInExpired);
  expect(navigation.reload).toHaveBeenCalledTimes(1);
});

test('a second sign-in within 30 s shows the banner instead of reloading again, and its Reload is the person\'s to press', async () => {
  render(<SignInBanner />);
  act(() => signIn());
  expect(navigation.reload).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole('alert')).toBeNull();
  vi.setSystemTime(Date.now() + 29_000);
  act(() => signIn());
  expect(navigation.reload).toHaveBeenCalledTimes(1);
  const banner = screen.getByRole('alert');
  expect(banner).toHaveTextContent('Sign in again');
  await userEvent.click(screen.getByRole('button', { name: 'Reload' }));
  expect(navigation.reload).toHaveBeenCalledTimes(2);
});

test('a sign-in after the window has passed reloads again', () => {
  signIn();
  vi.setSystemTime(Date.now() + 30_000);
  signIn();
  expect(navigation.reload).toHaveBeenCalledTimes(2);
  expect(signInStuck.get()).toBe(false);
});

test('a sign-in while text is typed shows the banner instead of reloading, so the draft is not lost', () => {
  render(<><textarea aria-label="Message" defaultValue="half a thought" /><SignInBanner /></>);
  act(() => signIn());
  expect(navigation.reload).not.toHaveBeenCalled();
  expect(screen.getByRole('alert')).toHaveTextContent('Sign in again');
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('half a thought');
});

test('the banner leaves once a call is answered again, as when another tab signed in', async () => {
  render(<SignInBanner />);
  act(() => signIn());
  act(() => signIn());
  expect(screen.getByRole('alert')).toBeInTheDocument();
  vi.stubGlobal('fetch', async () => new Response('{}'));
  await act(() => apiFetch('sessions'));
  expect(screen.queryByRole('alert')).toBeNull();
});

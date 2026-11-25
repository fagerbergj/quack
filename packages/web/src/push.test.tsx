import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import vm from 'node:vm';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import { Settings } from './Settings.tsx';

const source = readFileSync(resolve(__dirname, '../public/sw.js'), 'utf8');

// Runs the real worker file against a fake scope and returns what it asked the browser to do.
function worker(windows: Array<{ focus(): Promise<void>; navigate(url: string): Promise<void> }>) {
  const handlers: Record<string, (e: unknown) => void> = {};
  const shown: Array<[string, { body: string; data: { url: string } }]> = [];
  const opened: string[] = [];
  const self = {
    addEventListener: (type: string, fn: (e: unknown) => void) => { handlers[type] = fn; },
    registration: { scope: 'https://q.example/v1/', showNotification: async (t: string, o: never) => void shown.push([t, o]) },
    clients: { matchAll: async () => windows, openWindow: async (u: string) => void opened.push(u), claim: async () => {}, },
    skipWaiting: () => {},
  };
  vm.runInNewContext(source, { self, URL });
  const waits: Promise<unknown>[] = [];
  const fire = async (type: string, e: object) => { handlers[type]!({ ...e, waitUntil: (p: Promise<unknown>) => waits.push(p) }); await Promise.all(waits); };
  return { fire, shown, opened };
}

const click = (url: string) => ({ notification: { close: () => {}, data: { url } } });

test('a push shows its title and one line', async () => {
  const w = worker([]);
  await w.fire('push', { data: { json: () => ({ title: 'Approval waiting: Review #2095', body: '', url: '#/s/a' }) } });
  expect(w.shown).toEqual([['Approval waiting: Review #2095', expect.objectContaining({ data: { url: '#/s/a' } })]]);
});

test('a click focuses an open window and sends it to the session hash', async () => {
  const seen: string[] = [];
  const w = worker([{ focus: async () => void seen.push('focus'), navigate: async (u) => void seen.push(u) }]);
  await w.fire('notificationclick', click('#/s/github%3Ao%2Fr%231'));
  expect(seen).toEqual(['focus', 'https://q.example/v1/#/s/github%3Ao%2Fr%231']);
  expect(w.opened).toEqual([]);
});

test('a click opens the session when no window is open or the window cannot navigate', async () => {
  const none = worker([]);
  await none.fire('notificationclick', click('#/s/x'));
  const stuck = worker([{ focus: async () => {}, navigate: async () => { throw new Error('uncontrolled'); } }]);
  await stuck.fire('notificationclick', click('#/s/x'));
  expect([none.opened, stuck.opened]).toEqual([['https://q.example/v1/#/s/x'], ['https://q.example/v1/#/s/x']]);
});

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

test('Settings tells a Safari tab on iPhone to install instead of offering a toggle', async () => {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue('Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)');
  vi.stubGlobal('matchMedia', () => ({ matches: false }));
  render(<Settings />);
  expect(await screen.findByText(/add quack to your Home Screen/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Turn on' })).toBeNull();
});

type Fetch = (url: string, init?: RequestInit) => Promise<Response>;
const subscription = { endpoint: 'https://push.example/1', toJSON: () => ({ endpoint: 'https://push.example/1', keys: { p256dh: 'p', auth: 'a' } }), unsubscribe: async () => true };

// A browser that grants permission, with the given push API and server; Settings is rendered and Turn on clicked.
async function turnOn(server: Fetch, browser: { subscribe?: () => Promise<unknown>; register?: () => Promise<unknown> } = {}) {
  let subscribed: unknown = null;
  const pushManager = { getSubscription: async () => subscribed, subscribe: async () => (subscribed = await (browser.subscribe ?? (async () => subscription))()) };
  vi.stubGlobal('matchMedia', () => ({ matches: false }));
  vi.stubGlobal('PushManager', class {});
  vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => 'granted' });
  const reg = { pushManager };
  // A browser.register stands for a page with no worker yet, so turning on registers one.
  const getRegistration = async () => (browser.register ? undefined : reg);
  Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { getRegistration, register: browser.register, ready: Promise.resolve(reg) } });
  const calls: Array<[string, RequestInit | undefined]> = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => (url.startsWith('push/') ? (calls.push([url, init]), server(url, init)) : new Response(JSON.stringify({ models: [] }))));
  render(<Settings />);
  await userEvent.click(await screen.findByRole('button', { name: 'Turn on' }));
  return calls;
}

const key = () => new Response(JSON.stringify({ publicKey: 'BAAA' }));

test('turning notifications on subscribes and stores the subscription on the server', async () => {
  const calls = await turnOn(async (url) => (url === 'push/key' ? key() : new Response('{}')));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Turn off' })).toBeInTheDocument());
  expect(calls.map(([url, init]) => [url, init?.body && JSON.parse(String(init.body)), init?.redirect])).toEqual([
    ['push/key', undefined, 'manual'],
    ['push/subscribe', { subscription: { endpoint: 'https://push.example/1', keys: { p256dh: 'p', auth: 'a' } } }, 'manual'],
  ]);
});

const opaqueRedirect = { type: 'opaqueredirect', ok: false, status: 0 } as Response;
const problem = (detail: string) => new Response(JSON.stringify({ detail }), { status: 500 });

test.each([
  ['an expired sign-in', async () => opaqueRedirect, {}, 'Sign in again', 'Your sign-in expired, so quack could not get the push key. Reload to sign in, then try again.'],
  ['an unreachable server', async () => { throw new TypeError('Failed to fetch'); }, {}, "Can't reach quack", 'Could not get the push key: Failed to fetch. Check the connection, then try again.'],
  ['a worker that will not register', key, { register: async () => { throw new TypeError('The script resource is behind a redirect, which is disallowed.'); } }, 'Worker failed', 'Could not start the notification worker: The script resource is behind a redirect, which is disallowed. Reload, then try again.'],
  ['a refusing push service', key, { subscribe: async () => { throw new DOMException('Registration failed - push service error', 'AbortError'); } }, 'Push service refused', "Could not subscribe with the browser's push service: Registration failed - push service error. Check that this browser allows push messages, then try again."],
  ['a server that cannot store the device', async (url: string) => (url === 'push/key' ? key() : problem('subscription must have an https endpoint and keys')), {}, 'quack refused', 'Could not save this device: subscription must have an https endpoint and keys.'],
] as Array<[string, Fetch, Parameters<typeof turnOn>[1], string, string]>)('turning on with %s names the failed step and what to do', async (_, server, browser, shown, detail) => {
  await turnOn(server, browser);
  const alert = await screen.findByRole('alert');
  expect(alert).toHaveAttribute('title', detail);
  expect(alert).toHaveTextContent(`${shown}. ${detail}`);
  expect(screen.getByText(shown)).toHaveAttribute('aria-hidden', 'true');
  expect(screen.queryByRole('button', { name: 'Reload' }) !== null).toBe(shown === 'Sign in again');
  expect(screen.getByRole('button', { name: 'Turn on' })).toBeEnabled();
});

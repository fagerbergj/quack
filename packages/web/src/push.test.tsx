import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import vm from 'node:vm';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { navigation, signInStuck } from './auth.ts';
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

beforeEach(() => { sessionStorage.clear(); signInStuck.forget(); vi.spyOn(navigation, 'reload').mockImplementation(() => {}); });
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

test('Settings tells a Safari tab on iPhone to install instead of offering a toggle', async () => {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue('Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)');
  vi.stubGlobal('matchMedia', () => ({ matches: false }));
  render(<Settings />);
  expect(await screen.findByText(/add quack to your Home Screen/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Turn on' })).toBeNull();
});

type Fetch = (url: string, init?: RequestInit) => Promise<Response>;
type Browser = { subscribe?: () => Promise<unknown>; register?: () => Promise<unknown>; subscribed?: boolean };

// A browser that grants permission, with the given push API and server, on the Settings page; the push calls are returned as made.
function settings(server: Fetch, browser: Browser = {}) {
  const subscription = { endpoint: 'https://push.example/1', toJSON: () => ({ endpoint: 'https://push.example/1', keys: { p256dh: 'p', auth: 'a' } }), unsubscribe: vi.fn(async () => true) };
  let subscribed: unknown = browser.subscribed ? subscription : null;
  subscription.unsubscribe.mockImplementation(async () => ((subscribed = null), true));
  const pushManager = { getSubscription: async () => subscribed, subscribe: async () => (subscribed = await (browser.subscribe ?? (async () => subscription))()) };
  vi.stubGlobal('matchMedia', () => ({ matches: false }));
  vi.stubGlobal('PushManager', class {});
  vi.stubGlobal('Notification', { permission: browser.subscribed ? 'granted' : 'default', requestPermission: async () => 'granted' });
  const reg = { pushManager };
  // A browser.register stands for a page with no worker yet, so turning on registers one.
  const getRegistration = async () => (browser.register ? undefined : reg);
  Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { getRegistration, register: browser.register, ready: Promise.resolve(reg) } });
  const calls: Array<[string, RequestInit | undefined]> = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => (url.startsWith('push/') ? (calls.push([url, init]), server(url, init)) : new Response(JSON.stringify({ models: [] }))));
  render(<Settings />);
  return { calls, subscription };
}

const press = async (name: string) => userEvent.click(await screen.findByRole('button', { name }));
const key = async () => new Response(JSON.stringify({ publicKey: 'BAAA' }));
const ok = async () => new Response('{}');
const problem = (detail: string) => async () => new Response(JSON.stringify({ detail }), { status: 500 });
const offline = async () => { throw new TypeError('Failed to fetch'); };
// Signed out, the proxy redirects to its login origin: opaque under manual redirect, a CORS failure when followed.
const signedOut: Fetch = async (_url, init) => {
  if (init?.redirect === 'manual') return { type: 'opaqueredirect', ok: false, status: 0 } as Response;
  throw new TypeError('Failed to fetch');
};
const keyThen = (rest: Fetch): Fetch => async (url, init) => (url === 'push/key' ? key() : rest(url, init));

test('turning notifications on subscribes and stores the subscription on the server', async () => {
  const { calls } = settings(keyThen(ok));
  await press('Turn on');
  await waitFor(() => expect(screen.getByRole('button', { name: 'Turn off' })).toBeInTheDocument());
  expect(calls.map(([url, init]) => [url, init?.body && JSON.parse(String(init.body)), init?.redirect])).toEqual([
    ['push/key', undefined, 'manual'],
    ['push/subscribe', { subscription: { endpoint: 'https://push.example/1', keys: { p256dh: 'p', auth: 'a' } } }, 'manual'],
  ]);
});

test('a same-origin redirect is followed, not taken for an expired sign-in', async () => {
  // The followed request lands back on this origin, as a trailing-slash or https redirect does.
  settings(async (url, init) => (init?.redirect === 'manual' ? ({ type: 'opaqueredirect', ok: false, status: 0 } as Response) : url === 'push/key' ? key() : ok()));
  await press('Turn on');
  expect(await screen.findByRole('button', { name: 'Turn off' })).toBeInTheDocument();
  expect(navigation.reload).not.toHaveBeenCalled();
});

test('turning on with an expired sign-in sends the page to sign in, and leaves the panel without an error of its own', async () => {
  settings(signedOut);
  await press('Turn on');
  await waitFor(() => expect(navigation.reload).toHaveBeenCalledTimes(1));
  expect(screen.queryByRole('alert')).toBeNull();
  expect(screen.getByRole('button', { name: 'Turn on' })).toBeEnabled();
});

test.each([
  ['an unreachable server', offline, {}, "Can't reach quack. Check the connection, then try again.", 'Could not get the push key: Failed to fetch. Check the connection, then try again.'],
  ['a worker that will not register', key, { register: async () => { throw new TypeError('The script resource is behind a redirect, which is disallowed.'); } }, 'Worker failed. Reload, then try again.', 'Could not start the notification worker: The script resource is behind a redirect, which is disallowed. Reload, then try again.'],
  ['a refusing push service', key, { subscribe: async () => { throw new DOMException('Registration failed - push service error', 'AbortError'); } }, 'Push service refused. Check that this browser allows push messages, then try again.', "Could not subscribe with the browser's push service: Registration failed - push service error. Check that this browser allows push messages, then try again."],
  ['a server that cannot store the device', keyThen(problem('subscription must have an https endpoint and keys')), {}, 'quack refused. Try again later.', 'Could not save this device: subscription must have an https endpoint and keys. Try again later.'],
] as Array<[string, Fetch, Browser, string, string]>)('turning on with %s shows the failed step and what to do', async (_, server, browser, shown, detail) => {
  settings(server, browser);
  await press('Turn on');
  const alert = await screen.findByRole('alert');
  expect(alert).toHaveAttribute('title', detail);
  expect(screen.getByText(shown), 'the remedy is on screen, where touch can read it').toHaveAttribute('aria-hidden', 'true');
  expect(alert).toHaveTextContent(`${shown}${shown.split('. ')[0]}. ${detail}`);
  expect(screen.getByRole('button', { name: 'Turn on' })).toBeEnabled();
});

test.each([
  ['a server error', problem('database is locked')],
  ['an expired sign-in', signedOut],
])('turning off with %s still unsubscribes this browser', async (_, server) => {
  const { subscription, calls } = settings(server, { subscribed: true });
  await press('Turn off');
  expect(await screen.findByRole('button', { name: 'Turn on' })).toBeInTheDocument();
  expect(subscription.unsubscribe).toHaveBeenCalledTimes(1);
  expect(calls[0]?.[0]).toBe('push/unsubscribe');
  expect(screen.queryByRole('alert')).toBeNull();
});

test('a test push posts this device, and a failed one says which step and what to do', async () => {
  const { calls } = settings(ok, { subscribed: true });
  await press('Send a test');
  await waitFor(() => expect(calls.map(([url, init]) => [url, JSON.parse(String(init?.body))])).toEqual([['push/test', { endpoint: 'https://push.example/1' }]]));
  expect(screen.queryByRole('alert')).toBeNull();
  vi.stubGlobal('fetch', problem('this device is not subscribed'));
  await press('Send a test');
  expect(await screen.findByRole('alert')).toHaveAttribute('title', 'Could not send a test: this device is not subscribed. Try again later.');
  expect(screen.getByText('quack refused. Try again later.')).toBeInTheDocument();
  vi.stubGlobal('fetch', offline);
  await press('Send a test');
  await waitFor(() => expect(screen.getByText("Can't reach quack. Check the connection, then try again.")).toBeInTheDocument());
});

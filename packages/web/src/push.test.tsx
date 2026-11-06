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

test('turning notifications on subscribes and stores the subscription on the server', async () => {
  const stored: unknown[] = [];
  const subscription = { endpoint: 'https://push.example/1', toJSON: () => ({ endpoint: 'https://push.example/1', keys: { p256dh: 'p', auth: 'a' } }), unsubscribe: async () => true };
  let subscribed: unknown = null;
  const pushManager = { getSubscription: async () => subscribed, subscribe: async () => (subscribed = subscription) };
  vi.stubGlobal('matchMedia', () => ({ matches: false }));
  vi.stubGlobal('PushManager', class {});
  vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => 'granted' });
  Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { getRegistration: async () => ({ pushManager }) } });
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    if (url === 'push/subscribe') stored.push(JSON.parse(String(init?.body)));
    return new Response(JSON.stringify({ publicKey: 'BAAA' }));
  });
  render(<Settings />);
  await userEvent.click(await screen.findByRole('button', { name: 'Turn on' }));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Turn off' })).toBeInTheDocument());
  expect(stored).toEqual([{ subscription: { endpoint: 'https://push.example/1', keys: { p256dh: 'p', auth: 'a' } } }]);
});

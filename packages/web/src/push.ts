import { json } from './api.ts';

export type PushState = 'install' | 'unsupported' | 'blocked' | 'off' | 'on';

const post = (path: string, body: unknown) => fetch(path, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });

// iPadOS reports a Mac, so touch points tell them apart.
const onIos = () => /iP(hone|ad|od)/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
const installed = () => matchMedia('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true;

// Looking never registers the worker; only turning notifications on does.
const registration = () => navigator.serviceWorker.getRegistration().then((r) => r ?? navigator.serviceWorker.register('sw.js').then(() => navigator.serviceWorker.ready));
const current = async () => (await navigator.serviceWorker.getRegistration())?.pushManager.getSubscription() ?? null;

/** iOS only delivers push to the installed app, so a Safari tab is told to install rather than offered a toggle that cannot work. */
export async function pushState(): Promise<PushState> {
  if (onIos() && !installed()) return 'install';
  if (!('serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window)) return 'unsupported';
  if (Notification.permission === 'denied') return 'blocked';
  return Notification.permission === 'granted' && (await current().catch(() => null)) ? 'on' : 'off';
}

const keyBytes = (b64url: string) => Uint8Array.from(atob(b64url.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));

export async function enablePush(): Promise<PushState> {
  if ((await Notification.requestPermission()) !== 'granted') return pushState();
  const { publicKey } = await json<{ publicKey: string }>(await fetch('push/key'));
  const sub = await (await registration()).pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(publicKey) });
  await json(await post('push/subscribe', { subscription: sub.toJSON() }));
  return 'on';
}

export async function disablePush(): Promise<PushState> {
  const sub = await current();
  if (sub) {
    await post('push/unsubscribe', { endpoint: sub.endpoint });
    await sub.unsubscribe();
  }
  return 'off';
}

export async function testPush(): Promise<void> {
  const sub = await current();
  if (sub) await json(await post('push/test', { endpoint: sub.endpoint }));
}

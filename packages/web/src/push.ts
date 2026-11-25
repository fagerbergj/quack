import { apiFetch, json } from './api.ts';
import { SignInExpired } from './auth.ts';

export type PushState = 'install' | 'unsupported' | 'blocked' | 'off' | 'on';

/** A push step that failed: `message` is the terse line and `remedy` what to do, both on screen; `detail` adds which step and why. */
export class PushError extends Error {
  readonly remedy: string;
  readonly detail: string;
  constructor(message: string, cause: string, remedy: string) {
    super(message);
    this.remedy = remedy;
    this.detail = `${cause} ${remedy}`;
  }
}

// Browser messages may end in a period, and the detail adds its own after them.
const why = (err: unknown) => (typeof (err as Error | null)?.message === 'string' ? (err as Error).message : String(err)).replace(/\.$/, '');

// An expired sign-in passes through untouched: apiFetch has already sent the page to sign in.
async function request<T>(verb: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = body === undefined ? {} : { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) };
  const res = await apiFetch(path, init).catch((err: unknown) => {
    throw err instanceof SignInExpired ? err : new PushError("Can't reach quack", `Could not ${verb}: ${why(err)}.`, 'Check the connection, then try again.');
  });
  return json<T>(res).catch((err: unknown) => {
    throw new PushError('quack refused', `Could not ${verb}: ${why(err)}.`, 'Try again later.');
  });
}

async function guard<T>(message: string, verb: string, remedy: string, run: () => Promise<T>): Promise<T> {
  try {
    return await run();
  } catch (err) {
    throw new PushError(message, `Could not ${verb}: ${why(err)}.`, remedy);
  }
}

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
  const { publicKey } = await request<{ publicKey: string }>('get the push key', 'push/key');
  const reg = await guard('Worker failed', 'start the notification worker', 'Reload, then try again.', registration);
  const sub = await guard('Push service refused', "subscribe with the browser's push service", 'Check that this browser allows push messages, then try again.', () => reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(publicKey) }));
  await request('save this device', 'push/subscribe', { subscription: sub.toJSON() });
  return 'on';
}

export async function disablePush(): Promise<PushState> {
  const sub = await current();
  if (sub) {
    // The browser is the authority, so it unsubscribes first; a row the server keeps is dropped when the push service answers 410.
    await sub.unsubscribe();
    await request('remove this device', 'push/unsubscribe', { endpoint: sub.endpoint }).catch(() => undefined);
  }
  return 'off';
}

export async function testPush(): Promise<void> {
  const sub = await current();
  if (sub) await request('send a test', 'push/test', { endpoint: sub.endpoint });
}

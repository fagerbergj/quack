import { json } from './api.ts';

export type PushState = 'install' | 'unsupported' | 'blocked' | 'off' | 'on';

/** A push step that failed: `message` is the terse line on screen, `detail` names the step and what to do, and `signIn` offers a reload. */
export class PushError extends Error {
  readonly detail: string;
  readonly signIn: boolean;
  constructor(message: string, detail: string, signIn = false) {
    super(message);
    this.detail = detail;
    this.signIn = signIn;
  }
}

// Browser messages may end in a period, and the detail adds its own after them.
const why = (err: unknown) => (typeof (err as Error | null)?.message === 'string' ? (err as Error).message : String(err)).replace(/\.$/, '');

// An expired forward-auth sign-in redirects to the login origin, which fetch reports only as "Failed to fetch"; manual makes it visible.
async function call<T>(step: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = body === undefined ? {} : { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) };
  const res = await fetch(path, { ...init, redirect: 'manual' }).catch((err: unknown) => {
    throw new PushError("Can't reach quack", `Could not ${step}: ${why(err)}. Check the connection, then try again.`);
  });
  if (res.type === 'opaqueredirect') throw new PushError('Sign in again', `Your sign-in expired, so quack could not ${step}. Reload to sign in, then try again.`, true);
  return json<T>(res).catch((err: unknown) => {
    throw new PushError('quack refused', `Could not ${step}: ${why(err)}.`);
  });
}

async function step<T>(message: string, what: string, hint: string, run: () => Promise<T>): Promise<T> {
  try {
    return await run();
  } catch (err) {
    throw new PushError(message, `Could not ${what}: ${why(err)}. ${hint}`);
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
  const { publicKey } = await call<{ publicKey: string }>('get the push key', 'push/key');
  const reg = await step('Worker failed', 'start the notification worker', 'Reload, then try again.', registration);
  const sub = await step('Push service refused', "subscribe with the browser's push service", 'Check that this browser allows push messages, then try again.', () => reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(publicKey) }));
  await call('save this device', 'push/subscribe', { subscription: sub.toJSON() });
  return 'on';
}

export async function disablePush(): Promise<PushState> {
  const sub = await current();
  if (sub) {
    await call('remove this device', 'push/unsubscribe', { endpoint: sub.endpoint });
    await sub.unsubscribe();
  }
  return 'off';
}

export async function testPush(): Promise<void> {
  const sub = await current();
  if (sub) await call('send a test', 'push/test', { endpoint: sub.endpoint });
}

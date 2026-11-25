// The app sits behind a forward-auth proxy: an expired sign-in answers any request with a redirect to the login origin.
const STAMP = 'quack.signInAt';
const WINDOW_MS = 30_000;

/** Thrown by apiFetch when the sign-in expired; the page is already on its way to the login page, or the banner says so. */
export class SignInExpired extends Error {
  constructor() {
    super('Sign in again');
  }
}

/** The page navigation, a seam so tests can see it without leaving the page. */
export const navigation = { reload: () => location.reload() };

let stuck = false;
const listeners = new Set<() => void>();
/** Whether a sign-in navigation came straight back, so the page shows the banner instead of trying again; `forget` is for tests. */
export const signInStuck = {
  subscribe: (fn: () => void) => (listeners.add(fn), () => void listeners.delete(fn)),
  get: () => stuck,
  forget: () => void (stuck = false),
};

function lastTrip(): number | undefined {
  try {
    return Number(sessionStorage.getItem(STAMP)) || 0;
  } catch {
    return undefined;
  }
}

/** Reloads the whole page, so the proxy's login runs as a top-level navigation and returns here; a second trip within the window, or no storage to tell, shows the banner instead of looping. */
export function signIn(): void {
  const last = lastTrip();
  if (last !== undefined && Date.now() - last >= WINDOW_MS) {
    try {
      sessionStorage.setItem(STAMP, String(Date.now()));
      return navigation.reload();
    } catch {
      // Storage refused the stamp, so a reload could loop: fall through to the banner.
    }
  }
  stuck = true;
  listeners.forEach((fn) => fn());
}

// A same-origin redirect (a trailing slash, an https upgrade) is not a sign-in, so the opaque one is followed to see where it lands.
async function landsOffOrigin(path: string): Promise<boolean> {
  const probe = await fetch(path, { cache: 'no-store' }).catch(() => undefined);
  return !probe || new URL(probe.url, location.href).origin !== location.origin;
}

/** fetch for every call to quack: a redirect to another origin is an expired sign-in, which sends the page to sign in and throws SignInExpired. */
export async function apiFetch(path: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(path, { ...init, redirect: 'manual' });
  if (res.type !== 'opaqueredirect') return res;
  if (!(await landsOffOrigin(path))) return fetch(path, init);
  signIn();
  throw new SignInExpired();
}

/** For a stream that dropped (EventSource, WebSocket), which cannot see a redirect: one cheap request finds out whether the sign-in expired. */
export const probeSignIn = () => apiFetch('version', { cache: 'no-store' }).then(() => undefined, () => undefined);

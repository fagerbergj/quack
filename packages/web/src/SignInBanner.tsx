import { useSyncExternalStore } from 'react';
import { navigation, signInStuck } from './auth.ts';

const WHY = 'Your sign-in expired and signing in again did not bring the page back. Reload to sign in.';

/** Shown only when the automatic sign-in came straight back: the reload is left to the person, so the page never loops. */
export function SignInBanner() {
  const stuck = useSyncExternalStore(signInStuck.subscribe, signInStuck.get);
  if (!stuck) return null;
  return (
    <div role="alert" title={WHY} className="fixed inset-x-0 top-0 z-50 flex items-center justify-center gap-3 border-b border-line bg-surface px-4 py-2 text-[13.5px] text-danger shadow-card">
      <span aria-hidden="true">Sign in again</span>
      <span className="sr-only">{WHY}</span>
      <button type="button" onClick={() => navigation.reload()} className="min-h-11 cursor-pointer rounded-xl border border-line-strong bg-surface px-4 text-sm font-medium text-ink hover:bg-soft">Reload</button>
    </div>
  );
}

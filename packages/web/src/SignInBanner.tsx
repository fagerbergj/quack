import { useSyncExternalStore } from 'react';
import { navigation, signInStuck } from './auth.ts';

const WHY = 'Reload to sign in again: your sign-in to quack expired. Copy anything you are typing first, since a reload clears it.';

/** Shown when the page cannot reload to sign in by itself (text being typed, or a sign-in that came straight back), so the reload is the person's. */
export function SignInBanner() {
  const stuck = useSyncExternalStore(signInStuck.subscribe, signInStuck.get);
  if (!stuck) return null;
  return (
    <div role="alert" title={WHY} className="z-40 flex flex-none items-center justify-center gap-3 border-b border-line bg-surface px-4 pt-[calc(0.5rem+env(safe-area-inset-top,0px))] pb-2 text-[13.5px] text-danger lg:pl-72">
      <span aria-hidden="true">Sign in again</span>
      <span className="sr-only">{WHY}</span>
      <button type="button" onClick={() => navigation.reload()} className="min-h-11 cursor-pointer rounded-xl border border-line-strong bg-surface px-4 text-sm font-medium text-ink hover:bg-soft">Reload</button>
    </div>
  );
}

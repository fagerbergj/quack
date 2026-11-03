import { useEffect, useState } from 'react';
import { disablePush, enablePush, pushState, testPush, type PushState } from './push.ts';

const btn = 'min-h-11 cursor-pointer rounded-xl border border-line-strong bg-surface px-4 text-sm font-medium text-ink hover:bg-soft disabled:cursor-default disabled:opacity-50';

const NOTE: Record<PushState, string> = {
  install: 'On iPhone and iPad, add quack to your Home Screen, then open it from there to turn on notifications.',
  unsupported: 'This browser cannot send notifications.',
  blocked: 'Notifications are blocked for this site. Allow them in the browser settings, then come back.',
  off: 'Get a notification when something needs you.',
  on: 'This device gets a notification when something needs you.',
};

export function NotificationsSetting() {
  const [state, setState] = useState<PushState>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    pushState().then(setState, () => setState('unsupported'));
  }, []);
  const run = async (job: () => Promise<PushState | void>) => {
    setBusy(true);
    setError('');
    try {
      const next = await job();
      if (next) setState(next);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Something went wrong.');
    } finally {
      setBusy(false);
    }
  };
  return (
    <fieldset className="m-0 min-w-0 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
      <legend className="px-1 text-xs font-semibold tracking-wider text-muted uppercase">Notifications</legend>
      <p className="m-0 mb-2 text-[13.5px] text-ink-2">{state ? NOTE[state] : 'Checking this device.'}</p>
      <div className="flex flex-wrap gap-2">
        {state === 'off' && <button type="button" disabled={busy} onClick={() => run(enablePush)} className={btn}>Turn on</button>}
        {state === 'on' && (
          <>
            <button type="button" disabled={busy} onClick={() => run(testPush)} className={btn}>Send a test</button>
            <button type="button" disabled={busy} onClick={() => run(disablePush)} className={btn}>Turn off</button>
          </>
        )}
      </div>
      {error && <p role="alert" className="m-0 mt-2 text-[13.5px] text-danger [overflow-wrap:anywhere]">{error}</p>}
    </fieldset>
  );
}

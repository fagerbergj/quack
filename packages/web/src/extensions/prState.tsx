import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import type { Row } from '../sessions.ts';
import { useLoad, type Loaded } from '../useLoad.ts';
import { lookupPulls, type Pulls } from './pulls.ts';

const GITHUB_KEY = /^github:[^/]+\/[^#]+#\d+$/;
const PrStateContext = createContext<Loaded<Pulls>>({ data: {}, error: '' });
const NONE: Pulls = {};

// The service takes a webhook's merge at once and re-reads open PRs from GitHub every 10 min, so a minute here is the most a chip lags.
const REREAD_MS = 60_000;

// A merge changes no row, so the states are read again on return to the page and each minute while it is shown.
function useShownTick() {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const bump = () => setTick((t) => t + 1);
    let timer = document.visibilityState === 'visible' ? setInterval(bump, REREAD_MS) : undefined;
    const changed = () => {
      clearInterval(timer);
      timer = undefined;
      if (document.visibilityState !== 'visible') return;
      bump();
      timer = setInterval(bump, REREAD_MS);
    };
    document.addEventListener('visibilitychange', changed);
    return () => (clearInterval(timer), document.removeEventListener('visibilitychange', changed));
  }, []);
  return tick;
}

/** The github service's PR states for every listed review session, kept until a re-read answers; empty when the service is not connected. */
export function PrStates({ rows, children }: { rows: Row[] | undefined; children: ReactNode }) {
  const keys = (rows ?? []).filter((r) => r.state !== 'archived' &&GITHUB_KEY.test(r.key)).map((r) => r.key).sort().join(',');
  const tick = useShownTick();
  const pulls = useLoad(async () => (keys ? ((await lookupPulls(keys.split(','))) ?? NONE) : NONE), [keys, tick], NONE);
  return <PrStateContext value={pulls}>{children}</PrStateContext>;
}

/** Says so above the list when PR states could not be read, instead of leaving rows without a chip. */
export function PrStatesNotice() {
  const { error } = useContext(PrStateContext);
  return error ? <p role="alert" className="m-0 px-4 pt-3 text-[13px] text-danger">{`Pull request states are unavailable: ${error}`}</p> : null;
}

const CHIP = {
  open: { text: 'Open', cls: 'border border-line-strong text-ink-2' },
  merged: { text: 'Merged', cls: 'bg-merged-bg text-merged' },
  closed: { text: 'Closed', cls: 'bg-soft text-muted' },
};

/** Whether the pull request behind a github review session is open, merged or closed; nothing when unknown. */
export function PrChip({ sessionKey }: { sessionKey: string }) {
  const pr = useContext(PrStateContext).data[sessionKey.replace(/^github:/, '')];
  const chip = pr && Object.hasOwn(CHIP, pr.state) ? CHIP[pr.state] : undefined;
  if (!chip) return null;
  return <span title={`Pull request ${chip.text.toLowerCase()} ${new Date(pr!.at).toLocaleDateString()}`} className={`flex-none rounded-full px-2 text-[11px] leading-[18px] font-medium whitespace-nowrap ${chip.cls}`}>{chip.text}</span>;
}

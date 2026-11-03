import { createContext, useContext, useEffect, useState } from 'react';
import { json } from './api.ts';
import type { Row } from './sessions.ts';

export type PrState = { state: 'open' | 'merged' | 'closed'; at: string };
type Pulls = { [key: string]: PrState };

const GITHUB_KEY = /^github:[^/]+\/[^#]+#\d+$/;
const PrStateContext = createContext<Pulls>({});
export const PrStates = PrStateContext.Provider;

/** The github service's PR states for the listed review sessions, read once per set of keys; empty when the service is not connected. */
export function usePrStates(rows: Row[] | undefined): Pulls {
  const keys = (rows ?? []).filter((r) => !r.archived && GITHUB_KEY.test(r.key)).map((r) => r.key).sort().join(',');
  const [pulls, setPulls] = useState<Pulls>({});
  useEffect(() => {
    if (!keys) return;
    let live = true;
    fetch(`apps/github/pulls?keys=${encodeURIComponent(keys)}`)
      .then((res) => json<{ pulls: Pulls }>(res))
      .then((r) => live && setPulls(r.pulls), () => {});
    return () => {
      live = false;
    };
  }, [keys]);
  return pulls;
}

const CHIP = {
  open: { text: 'Open', cls: 'border border-line-strong text-ink-2' },
  merged: { text: 'Merged', cls: 'bg-merged-bg text-merged' },
  closed: { text: 'Closed', cls: 'bg-soft text-muted' },
};

/** Whether the pull request behind a github review session is open, merged or closed; nothing when unknown. */
export function PrChip({ sessionKey }: { sessionKey: string }) {
  const pr = useContext(PrStateContext)[sessionKey.replace(/^github:/, '')];
  const chip = pr && Object.hasOwn(CHIP, pr.state) ? CHIP[pr.state] : undefined;
  if (!chip) return null;
  return <span title={`Pull request ${chip.text.toLowerCase()} ${new Date(pr!.at).toLocaleDateString()}`} className={`flex-none rounded-full px-2 text-[11px] leading-[18px] font-medium whitespace-nowrap ${chip.cls}`}>{chip.text}</span>;
}

import { createContext, useContext, type ReactNode } from 'react';
import type { Row } from '../sessions.ts';
import { useLoad, type Loaded } from '../useLoad.ts';
import { lookupPulls, type Pulls } from './pulls.ts';

const GITHUB_KEY = /^github:([^/]+)\/([^#]+)#(\d+)$/;
const PrStateContext = createContext<Loaded<Pulls>>({ data: {}, error: '' });
const NONE: Pulls = {};

/** The github service's PR states for every listed review session, read once per set of keys; empty when the service is not connected. */
export function PrStates({ rows, children }: { rows: Row[] | undefined; children: ReactNode }) {
  const keys = (rows ?? []).filter((r) => r.state !== 'archived' &&GITHUB_KEY.test(r.key)).map((r) => r.key).sort().join(',');
  const pulls = useLoad(async () => (keys ? ((await lookupPulls(keys.split(','))) ?? NONE) : NONE), [keys], NONE);
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

/** The GitHub page of a github session's pull request; GitHub redirects /pull/<n> to /issues/<n> for a plain issue. */
export function prUrl(sessionKey: string) {
  const m = GITHUB_KEY.exec(sessionKey);
  return m ? `https://github.com/${m[1]}/${m[2]}/pull/${m[3]}` : undefined;
}

/** A link to the pull request behind a github session, saying whether it is open, merged or closed; just its number when that is unknown. */
export function PrChip({ sessionKey }: { sessionKey: string }) {
  const pr = useContext(PrStateContext).data[sessionKey.replace(/^github:/, '')];
  const url = prUrl(sessionKey);
  if (!url) return null;
  const n = `#${sessionKey.split('#').at(-1)}`;
  const known = pr && Object.hasOwn(CHIP, pr.state) ? CHIP[pr.state] : undefined;
  const chip = known ?? { text: n, cls: 'border border-line text-muted' };
  const state = known?.text.toLowerCase();
  return (
    // relative lifts it over the row link's overlay; the after: box widens the tap target past the 18 px chip.
    <a href={url} target="_blank" rel="noopener noreferrer" aria-label={state ? `Pull request ${n}, ${state}, on GitHub` : `${n} on GitHub`} title={state && `Pull request ${state} ${new Date(pr!.at).toLocaleDateString()}`} className={`relative flex flex-none items-center gap-0.5 rounded-full px-2 text-[11px] leading-[18px] font-medium whitespace-nowrap no-underline after:absolute after:-inset-x-1 after:-inset-y-1.5 hover:underline focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-link ${chip.cls}`}>
      {chip.text}
      <svg width="9" height="9" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round" aria-hidden="true"><path d="M8 4h12v12M20 4L5 19" /></svg>
    </a>
  );
}

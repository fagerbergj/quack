import { createContext, useContext, useEffect, useMemo, useRef, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import type { Row } from '../sessions.ts';
import { eventKey, pullKey, readPull, type Pull, type PullEvent } from './pulls.ts';

type Shown = { pulls: { [key: string]: Pull }; error: string };
type Live = Shown & { want: (key: string) => () => void; listed: ReadonlySet<string> };
const PrStateContext = createContext<Live>({ pulls: {}, error: '', want: () => () => {}, listed: new Set() });
// The app's Retry-After past its stream cap.
const REOPEN_MS = 30_000;

// The PRs chips on screen want, and the one EventSource on the app's pull_request stream that keeps them current.
function pullStream(set: Dispatch<SetStateAction<Shown>>) {
  const wanted = new Map<string, number>();
  const loaded = new Set<string>();
  let es: EventSource | undefined;
  let retry: ReturnType<typeof setTimeout> | undefined;
  const put = (key: string, pull: Pull) => (loaded.add(key), set((s) => ({ pulls: { ...s.pulls, [key]: pull }, error: '' })));
  const read = (key: string) => void readPull(key).then((pull) => pull && put(key, pull), (e: unknown) => set((s) => ({ ...s, error: e instanceof Error ? e.message : String(e) })));
  function open() {
    retry = undefined;
    es = new EventSource('apps/github/events');
    // Read after subscribing, so a change between the two is not lost; an event and a read are both absolute states, so applying both is harmless.
    es.onopen = () => wanted.forEach((_n, key) => read(key));
    es.addEventListener('pull_request', (m) => {
      const e = JSON.parse(m.data) as PullEvent;
      put(eventKey(e), e.pull_request);
    });
    es.onerror = failed;
  }
  function failed() {
    // Chips that waited for an open that has not come are read now; the rest keep their last state.
    wanted.forEach((_n, key) => loaded.has(key) || read(key));
    if (es?.readyState !== EventSource.CLOSED) return;
    // A non-200 answer (a 503 past the cap) ends an EventSource for good instead of retrying, so the page reopens it itself.
    es = undefined;
    retry = setTimeout(open, REOPEN_MS);
  }
  function close() {
    es?.close();
    clearTimeout(retry);
    es = retry = undefined;
  }
  function want(key: string) {
    wanted.set(key, (wanted.get(key) ?? 0) + 1);
    if (wanted.get(key) === 1 && !es && !retry) open();
    else if (wanted.get(key) === 1 && es?.readyState !== EventSource.CONNECTING) read(key);
    return () => {
      const n = wanted.get(key)! - 1;
      if (n) wanted.set(key, n);
      else wanted.delete(key);
      if (!wanted.size) close();
    };
  }
  return { want, close };
}

/** Live states for the PR badges on screen: each is read from the github app's GitHub mirror and kept current by one shared stream of its pull_request events. Archived sessions are not read. */
export function PrStates({ rows, children }: { rows: Row[] | undefined; children: ReactNode }) {
  const [shown, setShown] = useState<Shown>({ pulls: {}, error: '' });
  const live = useRef<ReturnType<typeof pullStream>>(null);
  live.current ??= pullStream(setShown);
  useEffect(() => () => live.current?.close(), []);
  const listed = useMemo(() => new Set((rows ?? []).filter((r) => r.state !== 'archived').map((r) => r.key)), [rows]);
  return <PrStateContext value={{ ...shown, want: live.current.want, listed }}>{children}</PrStateContext>;
}

/** Says so above the list when PR states could not be read, instead of leaving rows without a badge. */
export function PrStatesNotice() {
  const { error } = useContext(PrStateContext);
  return error ? <p role="alert" className="m-0 px-4 pt-3 text-[13px] text-danger">{`Pull request states are unavailable: ${error}`}</p> : null;
}

const KEY = /^github:([^/]+)\/([^#]+)#(\d+)$/;

/** The GitHub page of a github session's pull request, from its key, until the mirror answers with html_url. */
export function prUrl(sessionKey: string) {
  const m = KEY.exec(sessionKey);
  return m ? `https://github.com/${m[1]}/${m[2]}/pull/${m[3]}` : undefined;
}

const RINGS = 'M6 3.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5M6 15.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5M6 8.5v7';
const PR = `${RINGS}M18 15.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5M18 15.5V9a3 3 0 0 0-3-3h-4M13 3.5 10.5 6 13 8.5`;
// GitHub's pull request, merge and closed pull request marks, drawn as 24-unit strokes like the nav icons.
const BADGE = {
  open: { cls: 'border-transparent bg-success-bg text-success', icon: PR },
  merged: { cls: 'border-transparent bg-merged-bg text-merged', icon: `${RINGS}M18 9.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5M6 8.5c0 2 2 3.5 5 3.5h4.5` },
  closed: { cls: 'border-transparent bg-danger-bg text-danger', icon: `${RINGS}M18 15.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5M18 15.5V11M15.5 3.5l5 5M20.5 3.5l-5 5` },
  unknown: { cls: 'border-line-strong text-muted', icon: PR },
};

const stateOf = (pr: Pull | undefined) => (!pr ? 'unknown' : pr.merged ? 'merged' : pr.state);

/** One badge per pull request, GitHub-style: its state's icon and colour beside "#n", linking to the PR; neutral until its state is known. */
export function PrChip({ sessionKey }: { sessionKey: string }) {
  const { pulls, want, listed } = useContext(PrStateContext);
  const key = pullKey(sessionKey);
  const read = Boolean(key && listed.has(sessionKey));
  useEffect(() => (read ? want(key!) : undefined), [read, key, want]);
  const pr = key ? pulls[key] : undefined;
  const url = pr?.html_url ?? prUrl(sessionKey);
  if (!url) return null;
  const n = `#${sessionKey.split('#').at(-1)}`;
  const state = stateOf(pr);
  const at = pr?.merged_at ?? pr?.closed_at;
  return (
    // relative lifts it over the row link's overlay; the after: box takes the 18 px badge to a 30 px tap target. The border keeps every state one size.
    // The focus outline is drawn inside the pill: the session header's line 2 clips sideways (overflow-x), though not above or below.
    <a href={url} target="_blank" rel="noopener noreferrer" aria-label={pr ? `Pull request ${n}, ${state}, on GitHub` : `Pull request ${n} on GitHub`} title={pr ? `Pull request ${state}${at ? ` ${new Date(at).toLocaleDateString()}` : ''}` : undefined} className={`relative flex flex-none items-center gap-1 rounded-full border px-1.5 text-[11px] leading-4 font-medium tabular-nums whitespace-nowrap no-underline after:absolute after:-inset-x-1 after:-inset-y-1.5 hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-link ${BADGE[state].cls}`}>
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={BADGE[state].icon} /></svg>
      {n}
      <svg width="8" height="8" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round" aria-hidden="true"><path d="M8 4h12v12M20 4L5 19" /></svg>
    </a>
  );
}

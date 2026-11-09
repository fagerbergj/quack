import { createContext, useContext, useEffect, useRef, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { eventKey, pullKey, readPull, type Pull, type PullEvent } from './pulls.ts';

type Shown = { pulls: { [key: string]: Pull }; error: string };
type Live = Shown & { want: (key: string) => () => void };
const PrStateContext = createContext<Live>({ pulls: {}, error: '', want: () => () => {} });
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

/** Live states for the PR chips on screen: each is read from the github app's GitHub mirror and kept current by one shared stream of its pull_request events. */
export function PrStates({ children }: { children: ReactNode }) {
  const [shown, setShown] = useState<Shown>({ pulls: {}, error: '' });
  const live = useRef<ReturnType<typeof pullStream>>(null);
  live.current ??= pullStream(setShown);
  useEffect(() => () => live.current?.close(), []);
  return <PrStateContext value={{ ...shown, want: live.current.want }}>{children}</PrStateContext>;
}

/** Says so above the list when PR states could not be read, instead of leaving rows without a chip. */
export function PrStatesNotice() {
  const { error } = useContext(PrStateContext);
  return error ? <p role="alert" className="m-0 px-4 pt-3 text-[13px] text-danger">{`Pull request states are unavailable: ${error}`}</p> : null;
}

// Every chip has a border, so a live flip from Open does not shift the rows below by its width.
const CHIP = {
  open: { text: 'Open', cls: 'border border-line-strong text-ink-2' },
  merged: { text: 'Merged', cls: 'border border-transparent bg-merged-bg text-merged' },
  closed: { text: 'Closed', cls: 'border border-transparent bg-soft text-muted' },
};

/** Whether the pull request behind a github review session is open, merged or closed; nothing when unknown. */
export function PrChip({ sessionKey }: { sessionKey: string }) {
  const { pulls, want } = useContext(PrStateContext);
  const key = pullKey(sessionKey);
  useEffect(() => (key ? want(key) : undefined), [key, want]);
  const pr = key ? pulls[key] : undefined;
  if (!pr) return null;
  const chip = CHIP[pr.merged ? 'merged' : pr.state];
  const at = pr.merged_at ?? pr.closed_at;
  return <span title={`Pull request ${chip.text.toLowerCase()}${at ? ` ${new Date(at).toLocaleDateString()}` : ''}`} className={`flex-none rounded-full px-2 text-[11px] leading-[18px] font-medium whitespace-nowrap ${chip.cls}`}>{chip.text}</span>;
}

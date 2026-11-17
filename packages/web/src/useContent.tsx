import { useEffect, useState, type ReactNode } from 'react';
import { versionContent, type SealedArtifact } from './api.ts';

// A sealed version never changes, so its content is kept by hash for the page's life; concurrent readers share one request.
const kept = new Map<string, string>();
const pending = new Map<string, Promise<string>>();

/** For tests: a page lives long enough that a kept version never changes, but each test starts clean. */
export const forgetContent = () => (kept.clear(), pending.clear());

/** The content of one sealed version, from the Sessions API; the live socket carries it for a kind's newest version only. */
export function loadContent(sessionKey: string, a: SealedArtifact): Promise<string> {
  const hit = kept.get(a.hash);
  if (hit !== undefined) return Promise.resolve(hit);
  let p = pending.get(a.hash);
  if (!p) {
    p = versionContent(sessionKey, a.kind, a.version).then((c) => (kept.set(a.hash, c), c)).finally(() => pending.delete(a.hash));
    pending.set(a.hash, p);
  }
  return p;
}

export type Content = { content?: string; loading: boolean; error: string; retry: () => void };

/** A version's content: the row's own when the socket carried it, else loaded on demand. `a` undefined asks for nothing. */
export function useContent(sessionKey: string, a: SealedArtifact | undefined): Content {
  const [outcome, setOutcome] = useState<{ hash: string; error: string }>();
  const [tick, setTick] = useState(0);
  const need = a?.content === undefined ? a : undefined;
  useEffect(() => {
    if (!need) return;
    let current = true;
    loadContent(sessionKey, need).then(
      () => current && setOutcome({ hash: need.hash, error: '' }),
      (e: unknown) => current && setOutcome({ hash: need.hash, error: e instanceof Error ? e.message : String(e) }),
    );
    return () => void (current = false);
  }, [sessionKey, need?.hash, tick]);
  const content = a?.content ?? (a && kept.get(a.hash));
  const error = content === undefined && outcome && outcome.hash === a?.hash ? outcome.error : '';
  return { content, loading: Boolean(need) && content === undefined && !error, error, retry: () => (setOutcome(undefined), setTick((t) => t + 1)) };
}

/** What stands in for a version's content until it is there: a plain loading line, or the failure with a retry. */
export function ContentState({ what, loading, error, retry }: { what: string; loading: boolean; error: string; retry: () => void }) {
  if (error) return <p role="alert" className="m-0 text-[13px] text-danger">{`${what} could not be loaded: ${error} `}<button type="button" onClick={retry} className="min-h-6 cursor-pointer border-0 bg-transparent p-0 text-link underline">Retry</button></p>;
  return loading ? <p role="status" className="m-0 text-[13px] text-muted">{`Loading ${what}…`}</p> : null;
}

/** Draws `children` with the version's content once it is there. */
export function Loaded({ sessionKey, a, what, children }: { sessionKey: string; a: SealedArtifact; what: string; children: (a: SealedArtifact) => ReactNode }) {
  const c = useContent(sessionKey, a);
  return c.content === undefined ? <ContentState what={what} {...c} /> : children({ ...a, content: c.content });
}

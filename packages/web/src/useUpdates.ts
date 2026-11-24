import { useEffect, useRef } from 'react';
import { getVersion, type Version } from './api.ts';

const TEXT = new Set(['text', 'search', 'url', 'email', 'number', 'textarea']);

/** True when a reload would lose something: a typed field, an open dialog, or anything marked data-unsaved (pending attachments). */
export function unsaved(doc: Document = document) {
  if (doc.querySelector('[role="dialog"], [data-unsaved]')) return true;
  return [...doc.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')].some((el) => TEXT.has(el.type) && el.value.trim() !== '');
}

/** Whether the server now runs a different build than this page loaded with; an unknown build on either side is no change. */
export const outdated = (running: string | null | undefined, served: string | null | undefined) => Boolean(running && served && running !== served);

/** Reloads into `served`, the build the server's change stream names, when it differs from this page's: at once when nothing is unsaved,
 * else at the next moment the page is hidden and clean. */
export function useUpdates(served: Version | undefined, reload: () => void = () => location.reload()) {
  const stale = useRef(false);
  const settle = useRef(() => { if (stale.current && !unsaved()) reload(); });
  useEffect(() => {
    let live = true;
    void getVersion().then((running) => {
      stale.current ||= outdated(running?.commit, served?.commit);
      if (live && document.visibilityState === 'visible') settle.current();
    });
    return () => void (live = false);
  }, [served]);
  useEffect(() => {
    const seen = () => settle.current();
    document.addEventListener('visibilitychange', seen);
    return () => document.removeEventListener('visibilitychange', seen);
  }, []);
}

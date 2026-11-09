import { useEffect } from 'react';
import { getVersion, servedVersion } from './api.ts';

// An installed app stays open for hours, so it asks the server for its build instead of waiting for a fresh load.
const EVERY = 5 * 60_000;
const TEXT = new Set(['text', 'search', 'url', 'email', 'number', 'textarea']);

/** True when a reload would lose something: a typed field, an open dialog, or anything marked data-unsaved (pending attachments). */
export function unsaved(doc: Document = document) {
  if (doc.querySelector('[role="dialog"], [data-unsaved]')) return true;
  return [...doc.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')].some((el) => TEXT.has(el.type) && el.value.trim() !== '');
}

/** Whether the server now runs a different build than this page loaded with; an unknown build on either side is no change. */
export const outdated = (running: string | null | undefined, served: string | null | undefined) => Boolean(running && served && running !== served);

/** Reloads into a new deploy: at once when nothing is unsaved, else at the next moment the page is hidden and clean. */
export function useUpdates(reload: () => void = () => location.reload()) {
  useEffect(() => {
    const running = getVersion();
    let stale = false;
    const settle = () => { if (stale && !unsaved()) reload(); };
    const check = async () => {
      stale ||= outdated((await running)?.commit, (await servedVersion())?.commit);
      if (document.visibilityState === 'visible') settle();
    };
    const seen = () => void (document.visibilityState === 'visible' ? check() : settle());
    const timer = setInterval(() => document.visibilityState === 'visible' && void check(), EVERY);
    document.addEventListener('visibilitychange', seen);
    return () => { clearInterval(timer); document.removeEventListener('visibilitychange', seen); };
  }, []);
}

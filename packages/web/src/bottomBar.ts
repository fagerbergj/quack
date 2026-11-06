import { useEffect, useState } from 'react';
import { EXTENSIONS } from './extensions/index.ts';

export type Item = { route: string; label: string; href: string };

export const HOME: Item = { route: 'home', label: 'Home', href: '#/' };
export const CORE: Item[] = [
  { route: 'sessions', label: 'Sessions', href: '#/sessions' },
  { route: 'artifacts', label: 'Artifacts', href: '#/artifacts' },
  { route: 'agents', label: 'Agents', href: '#/agents' },
];
// The pages the bottom bar can hold; extension pages join as they register.
export const PAGES: Item[] = [...CORE, ...EXTENSIONS.flatMap((e) => e.nav ?? []).map((n): Item => ({ route: n.route, label: n.label, href: `#/${n.route}` }))];
export const MAX_PICKS = 3;

const KEY = 'quack:bottom-bar';
const CHANGED = 'quack:bottom-bar-changed';
// Agents is a choice, never a default. Pinned extension pages follow Sessions and Artifacts; the next available page fills any room left.
const PINNED = EXTENSIONS.flatMap((e) => (e.nav?.pinned ? [e.nav.route] : []));
const DEFAULT = [...new Set(['sessions', 'artifacts', ...PINNED, ...PAGES.map((p) => p.route).filter((r) => r !== 'agents')])].slice(0, MAX_PICKS);

// Per device; unreadable or stale storage reads as the default.
export function storedBar(): string[] {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(KEY) ?? 'null');
    if (Array.isArray(v)) return [...new Set(v)].filter((r): r is string => PAGES.some((p) => p.route === r)).slice(0, MAX_PICKS);
  } catch {
    // Blocked storage or bad JSON: use the default.
  }
  return DEFAULT;
}

export function saveBar(routes: string[]) {
  try {
    localStorage.setItem(KEY, JSON.stringify(routes));
  } catch {
    // Private mode: the choice holds until the page reloads.
  }
  dispatchEvent(new CustomEvent(CHANGED, { detail: routes }));
}

export function useBar(): string[] {
  const [bar, setBar] = useState(storedBar);
  useEffect(() => {
    const on = (e: Event) => setBar((e as CustomEvent<string[]>).detail);
    addEventListener(CHANGED, on);
    return () => removeEventListener(CHANGED, on);
  }, []);
  return bar;
}

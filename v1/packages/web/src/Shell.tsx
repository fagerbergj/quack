import { useEffect, type ReactNode } from 'react';
import { CORE, HOME, PAGES, useBar, type Item } from './bottomBar.ts';
import { EXTENSIONS } from './extensions/index.ts';

export type Route = string;

// Icon paths from the mockup's ICONS table.
const ICONS: Record<string, string> = {
  home: 'M4 11l8-6 8 6v8a1 1 0 0 1-1 1h-4v-5h-6v5H5a1 1 0 0 1-1-1z',
  sessions: 'M3 12h4l3 8 4-16 3 8h4',
  artifacts: 'M14 3H6v18h12V7zM14 3v4h4M9 13h6M9 17h6',
  settings: 'M4 7h10M18 7h2M4 17h2M10 17h10M16 4v6M8 14v6',
  grid: 'M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z',
};

export const MAIN: Item[] = [HOME, ...CORE];
export const GROUPS: { title: string; items: Item[] }[] = [
  { title: 'Apps', items: [
    ...EXTENSIONS.flatMap((e) => e.nav ?? []).map((n): Item => ({ route: n.route, label: n.label, href: `#/${n.route}` })),
  ] },
  { title: 'Settings', items: [
    { route: 'settings', label: 'Settings', href: '#/settings' },
  ] },
];
const TITLES = Object.fromEntries([{ route: 'more', label: 'More' }, ...MAIN, ...GROUPS.flatMap((g) => g.items)].map((n) => [n.route, n.label])) as Record<Route, string>;
const NAV_ICONS = Object.fromEntries(EXTENSIONS.flatMap((e) => e.nav ?? []).map((n) => [n.route, n.icon]));
export const MORE = GROUPS.flatMap((g) => g.items);

// The logo duck's shapes in currentColor, so the Agents tab colours like the stroked icons; the eye is cut out in the canvas colour.
function DuckGlyph() {
  return (
    <svg width="16" height="16" viewBox="4 11 80 80" fill="currentColor" aria-hidden="true" className="flex-none">
      <ellipse cx="52" cy="62" rx="26" ry="14" />
      <circle cx="38" cy="40" r="14" />
      <path d="M26 37 L9 42 L26 47 Z" />
      <path d="M70 56 Q80 52 78 62" />
      <circle cx="34" cy="36" r="3" style={{ fill: 'var(--color-canvas)' }} />
    </svg>
  );
}

export function Icon({ route }: { route: Route }) {
  if (route === 'agents') return <DuckGlyph />;
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="flex-none">
      <path d={ICONS[route] ?? NAV_ICONS[route]} />
    </svg>
  );
}

function Duck({ size, className = '' }: { size: number; className?: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 96 96" aria-hidden="true" className={className}>
      <rect width="96" height="96" rx="26" fill="#1f7a63" />
      <ellipse cx="52" cy="62" rx="26" ry="14" fill="#fff" />
      <circle cx="38" cy="40" r="14" fill="#fff" />
      <path d="M26 37 L9 42 L26 47 Z" fill="#e3a92b" />
      <circle cx="34" cy="36" r="2.6" fill="#0f1513" />
      <path d="M70 56 Q80 52 78 62" fill="#fff" />
    </svg>
  );
}

export const THEME_KEY = 'quack:theme';

export function storedTheme() {
  try {
    return localStorage.getItem(THEME_KEY);
  } catch {
    return null;
  }
}

const linkBase = 'navlink flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-ink-2 no-underline hover:bg-soft hover:text-ink';
const tabBase = 'tablink flex min-h-11 flex-col items-center justify-center gap-0.5 py-2 text-[11px] font-medium text-muted no-underline';

// Below lg main is the only scroller, so the document never scrolls and the URL bar cannot collapse mid-scroll; `bare` drops the phone title bar.
// `sidebar` is the session list; it fills the desktop sidebar under a fixed nav and scrolls on its own.
export function Shell({ route, bare, sidebar, children }: { route: Route; bare?: boolean; sidebar?: ReactNode; children: ReactNode }) {
  const bar = useBar();
  const tabs = bar.flatMap((r) => PAGES.find((p) => p.route === r) ?? []);
  // The sidebar honours the same picks as the bottom bar; the rest sit under More, as on the More page.
  const rest = [...CORE, ...MORE].filter((n) => !bar.includes(n.route));
  const cur = (r: Route) => (r === route ? ('page' as const) : undefined);
  const link = (n: Item, cls: string) => (
    <a key={n.route} href={n.href} aria-current={cur(n.route)} className={cls}>
      <Icon route={n.route} />
      {n.label}
    </a>
  );
  // A stored choice wins over the OS preference, which applies when nothing is stored.
  useEffect(() => {
    const t = storedTheme();
    if (t) document.documentElement.dataset.theme = t;
  }, []);
  return (
    <>
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-72 flex-col border-r border-line bg-canvas/90 backdrop-blur-md lg:flex">
        <a href="#/" className="logo-link wordmark relative flex items-center gap-2.5 px-5 pt-5 pb-3 text-[19px] text-ink no-underline">
          <span aria-hidden="true" className="quack-bubble absolute top-[-2px] left-[54px] rounded-full rounded-bl-none bg-warm px-2 text-[13px] text-warning">quack!</span>
          <Duck size={30} className="bob" />
          quack
        </a>
        <a href="#/new" className="mx-3 mb-2 inline-flex h-9 items-center justify-center rounded-full bg-ink px-3 text-[13px] font-semibold text-on-ink no-underline hover:bg-ink-2">New session</a>
        <nav aria-label="Main" className="flex flex-none flex-col gap-0.5 px-3 pb-2 text-[13.5px]">
          {[HOME, ...tabs].map((n) => link(n, linkBase))}
          <details className="group">
            <summary className="flex cursor-pointer list-none items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-ink-2 hover:bg-soft hover:text-ink [&::-webkit-details-marker]:hidden">
              <Icon route="grid" />
              More
            </summary>
            <div className="flex flex-col gap-0.5 pl-3">{rest.map((n) => link(n, linkBase))}</div>
          </details>
        </nav>
        <div className="flex min-h-0 flex-1 flex-col border-t border-line">{sidebar}</div>
      </aside>
      <div className="fixed inset-x-0 top-0 bottom-[calc(48px+var(--safe-b))] flex flex-col lg:contents">
      <header className={`flex-none z-20 ${bare ? 'hidden' : 'flex'} items-center gap-2.5 border-b border-line bg-canvas/85 px-4 pt-[calc(0.875rem+env(safe-area-inset-top,0px))] pb-3.5 backdrop-blur-md lg:hidden`}>
        <Duck size={30} />
        {route === 'home' ? <span className="wordmark text-xl">quack</span> : route === 'sessions' ? <h1 className="m-0 text-[19px]">Sessions</h1> : <span className="text-[15px] font-semibold tracking-[-0.015em]">{TITLES[route]}</span>}
        {route === 'sessions' && (
          <a href="#/new" aria-label="New session" className="ml-auto grid h-11 w-11 place-items-center rounded-full bg-ink text-on-ink no-underline hover:bg-ink-2">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
          </a>
        )}
      </header>
      <main className="min-h-0 flex-1 overflow-y-auto overscroll-y-contain lg:min-h-screen lg:overflow-visible lg:pl-72">{children}</main>
      </div>
      <nav aria-label="Main" className="fixed inset-x-0 bottom-0 z-30 grid h-[calc(48px+var(--safe-b))] border-t border-line bg-canvas/90 pb-[var(--safe-b)] backdrop-blur-md lg:hidden" style={{ gridTemplateColumns: `repeat(${tabs.length + 2}, minmax(0, 1fr))` }}>
        {[HOME, ...tabs].map((n) => link(n, tabBase))}
        <a href="#/more" aria-current={route === 'more' || (route !== HOME.route && !tabs.some((n) => n.route === route)) ? 'page' : undefined} className={tabBase}>
          <Icon route="grid" />
          More
        </a>
      </nav>
    </>
  );
}

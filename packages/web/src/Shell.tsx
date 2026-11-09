import { useEffect, type ReactNode } from 'react';
import { CORE, HOME, PAGES, useBar, type Item } from './bottomBar.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { shortCommit, useVersion } from './useVersion.ts';

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

// The logo's shapes, shared with public/favicon.svg, which tools/icons.ts renders to the PNG icons.
const BODY = 'M38 22c9.4 0 16 6.6 16 15.2 0 4.3-1.6 7.9-4.3 10.8 5.6-.3 11.4-1.5 16.6-3.8 4.6-2 8.6-4.9 11.6-8.4.9-1 2.5-.5 2.6.9.6 7.5-.4 15.6-4 22.4C71.4 70.6 61.6 76 49 76H38c-11.9 0-20-6.9-20-16.4 0-6.1 3.4-10.9 8.6-13.6-2.8-2.9-4.6-6.6-4.6-10.6C22 28.4 29 22 38 22z';
const BILL = 'M23.6 33.2c-4.7-.5-9.6.3-12.8 2.2-1.6 1-1.5 3.1.2 3.9 3.6 1.7 8.4 2 12.8 1.3z';
const WING = 'M44 60c6 4.6 15.2 5 22.6.2';

// The duck without its tile, in currentColor like the stroked icons; the eye and wing are cut out in the canvas colour.
function DuckGlyph() {
  return (
    <svg width="16" height="16" viewBox="9.5 13 72 72" fill="currentColor" aria-hidden="true" className="flex-none">
      <path d={BODY} />
      <path d={BILL} />
      <circle cx="36.5" cy="32.5" r="2.7" style={{ fill: 'var(--color-canvas)' }} />
      <path d={WING} fill="none" style={{ stroke: 'var(--color-canvas)' }} strokeWidth="3.6" strokeLinecap="round" />
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
      <rect width="96" height="96" rx="24" fill="#1f7a63" />
      <path d={BODY} fill="#fff" />
      <path d={BILL} fill="#e3a92b" />
      <circle cx="36.5" cy="32.5" r="2.7" fill="#0f1513" />
      <path d={WING} fill="none" stroke="#1f7a63" strokeWidth="3.6" strokeLinecap="round" />
    </svg>
  );
}

// Links to Settings, where the full version and build time live.
export function VersionLink({ className }: { className: string }) {
  const v = useVersion();
  return v?.commit ? <a href="#/settings" className={className}>{`Version ${shortCommit(v.commit)}`}</a> : null;
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
        <nav aria-label="Main" className="flex flex-none flex-col gap-0.5 px-3 pb-2 text-[13.5px]">
          {[HOME, ...tabs].map((n) => link(n, linkBase))}
          {/* Opens on its own when the current page is in it, so the rail always shows where you are. */}
          <details className="group" open={rest.some((n) => n.route === route) || undefined}>
            <summary className="flex cursor-pointer list-none items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-ink-2 hover:bg-soft hover:text-ink [&::-webkit-details-marker]:hidden">
              <Icon route="grid" />
              More
            </summary>
            <div className="flex flex-col gap-0.5 pl-3">{rest.map((n) => link(n, linkBase))}</div>
          </details>
        </nav>
        <div className="flex min-h-0 flex-1 flex-col border-t border-line">{sidebar}</div>
        <VersionLink className="px-5 py-2 font-mono text-[11px] text-muted no-underline hover:text-ink" />
      </aside>
      <div className="fixed inset-x-0 top-0 bottom-[calc(48px+var(--safe-b))] flex flex-col lg:contents">
      <header className={`flex-none z-20 ${bare ? 'hidden' : 'flex'} items-center gap-2.5 border-b border-line bg-canvas/85 px-4 pt-[calc(0.875rem+env(safe-area-inset-top,0px))] pb-3.5 backdrop-blur-md lg:hidden`}>
        <Duck size={30} />
        {route === 'home' ? <span className="wordmark text-xl">quack</span> : route === 'sessions' ? <h1 className="m-0 text-[19px]">Sessions</h1> : <span className="text-[15px] font-semibold tracking-[-0.015em]">{TITLES[route]}</span>}
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

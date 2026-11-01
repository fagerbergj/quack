import { useEffect, useState, type ReactNode } from 'react';

export type Route = 'sessions' | 'inbox' | 'artifacts' | 'sleeper' | 'remarkable' | 'memory' | 'agents' | 'evals' | 'schedules';

// Icon paths from the mockup's ICONS table.
const ICONS: Record<Route | 'grid', string> = {
  sessions: 'M3 12h4l3 8 4-16 3 8h4',
  inbox: 'M3 13h5l2 3h4l2-3h5M5 5h14l2 8v6H3v-6z',
  artifacts: 'M14 3H6v18h12V7zM14 3v4h4M9 13h6M9 17h6',
  sleeper: 'M8 4h8v5a4 4 0 0 1-8 0zM12 13v4M8 21h8M16 6h3v2a3 3 0 0 1-3 3M8 6H5v2a3 3 0 0 0 3 3',
  remarkable: 'M4 20l4-1 11-11-3-3L5 16zM14 6l3 3',
  memory: 'M6 3h12v18l-6-4-6 4z',
  agents: 'M4 8h16v11H4zM12 4v4M9 13h.01M15 13h.01',
  evals: 'M9 3h6M10 3v6l-5 9a2 2 0 0 0 2 3h10a2 2 0 0 0 2-3l-5-9V3',
  schedules: 'M20 12a8 8 0 1 1-16 0 8 8 0 0 1 16 0zM12 8v4l3 2',
  grid: 'M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z',
};

type Item = { route: Route; label: string; href: string };
export const MAIN: Item[] = [
  { route: 'sessions', label: 'Sessions', href: '#/' },
  { route: 'inbox', label: 'Inbox', href: '#/inbox' },
  { route: 'artifacts', label: 'Artifacts', href: '#/artifacts' },
];
export const GROUPS: { title: string; items: Item[] }[] = [
  { title: 'Apps', items: [
    { route: 'sleeper', label: 'Sleeper', href: '#/sleeper' },
    { route: 'remarkable', label: 'reMarkable', href: '#/remarkable' },
    { route: 'memory', label: 'Memory', href: '#/memory' },
  ] },
  { title: 'Operate', items: [
    { route: 'agents', label: 'Agents', href: '#/agents' },
    { route: 'evals', label: 'Evals', href: '#/evals' },
    { route: 'schedules', label: 'Schedules', href: '#/schedules' },
  ] },
];
const TITLES = Object.fromEntries([...MAIN, ...GROUPS.flatMap((g) => g.items)].map((n) => [n.route, n.label])) as Record<Route, string>;
const MORE = GROUPS.flatMap((g) => g.items);

function Icon({ route }: { route: Route | 'grid' }) {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="flex-none">
      <path d={ICONS[route]} />
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

const THEME_KEY = 'quack:theme';

function storedTheme() {
  try {
    return localStorage.getItem(THEME_KEY);
  } catch {
    return null;
  }
}

const readTheme = () => document.documentElement.dataset.theme ?? storedTheme() ?? (globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');

function ThemeToggle({ className }: { className: string }) {
  const [, rerender] = useState(0);
  const toggle = () => {
    const next = readTheme() === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch {
      // Private mode or blocked storage: the choice just won't persist.
    }
    rerender((n) => n + 1);
  };
  return (
    <button type="button" onClick={toggle} className={`${className} flex min-h-11 cursor-pointer items-center rounded-lg font-medium text-muted hover:bg-soft hover:text-ink`} aria-pressed={readTheme() === 'dark'}>
      Day / night
    </button>
  );
}

const linkBase = 'navlink flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-ink-2 no-underline hover:bg-soft hover:text-ink';
const tabBase = 'tablink flex min-h-11 flex-col items-center justify-center gap-0.5 py-2 text-[11px] font-medium text-muted no-underline';

// `bare` drops the phone title bar for a screen that draws its own.
// `sidebar` is the session list; it fills the desktop sidebar under a fixed nav and scrolls on its own.
export function Shell({ route, bare, sidebar, children }: { route: Route; bare?: boolean; sidebar?: ReactNode; children: ReactNode }) {
  const [more, setMore] = useState(false);
  const cur = (r: Route) => (r === route ? ('page' as const) : undefined);
  const link = (n: Item, cls: string) => (
    <a key={n.route} href={n.href} aria-current={cur(n.route)} className={cls} onClick={() => setMore(false)}>
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
          {MAIN.map((n) => link(n, linkBase))}
          <details className="group">
            <summary className="flex cursor-pointer list-none items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-ink-2 hover:bg-soft hover:text-ink [&::-webkit-details-marker]:hidden">
              <Icon route="grid" />
              More
            </summary>
            <div className="flex flex-col gap-0.5 pl-3">{MORE.map((n) => link(n, linkBase))}</div>
          </details>
        </nav>
        <div className="flex min-h-0 flex-1 flex-col border-t border-line">{sidebar}</div>
        <div className="flex items-center gap-2.5 border-t border-line px-5 py-3 text-[13px]">
          <ThemeToggle className="ml-auto px-2 text-xs" />
        </div>
      </aside>
      <header className={`sticky top-0 z-20 ${bare ? 'hidden' : 'flex'} items-center gap-2.5 border-b border-line bg-canvas/85 px-4 pt-[calc(0.625rem+env(safe-area-inset-top,0px))] pb-2.5 backdrop-blur-md lg:hidden`}>
        <Duck size={24} />
        <span className="text-[15px] font-semibold tracking-[-0.015em]">{TITLES[route]}</span>
      </header>
      <main className="min-h-screen pb-24 lg:pb-0 lg:pl-72">{children}</main>
      <nav aria-label="Main" className="fixed inset-x-0 bottom-0 z-30 grid grid-cols-4 border-t border-line bg-canvas/90 pb-[env(safe-area-inset-bottom,0px)] backdrop-blur-md lg:hidden">
        {MAIN.map((n) => link(n, tabBase))}
        <button type="button" aria-expanded={more} aria-controls="more-sheet" aria-current={MORE.some((n) => n.route === route) ? 'page' : undefined} onClick={() => setMore(!more)} className={`${tabBase} cursor-pointer border-0 bg-transparent`}>
          <Icon route="grid" />
          More
        </button>
      </nav>
      <div id="more-sheet" hidden={!more} className="fixed inset-x-3 bottom-[calc(62px+env(safe-area-inset-bottom,0px))] z-30 rounded-[22px] border border-line bg-surface p-2 shadow-lift lg:hidden">
        {MORE.map((n) => link(n, 'flex min-h-11 items-center gap-3 rounded-lg px-3 text-[14px] text-ink no-underline hover:bg-soft'))}
        <ThemeToggle className="w-full gap-3 px-3 text-[14px]" />
      </div>
    </>
  );
}

const reed = 'M8 58 Q6 30 9 6';
const duckling = (
  <svg width="20" height="16" viewBox="0 0 18 14">
    <ellipse cx="10" cy="9" rx="7" ry="4.5" fill="#e9c46a" />
    <circle cx="5.5" cy="5" r="3.5" fill="#e9c46a" />
    <path d="M2.4 4.3 L0 5.2 L2.4 6.2 Z" fill="#d08a1d" />
    <circle cx="4.4" cy="4.2" r="0.8" fill="#0f1513" />
  </svg>
);

// The swimmer's head and bill point left in the source art; .swimmer flips it to face its rightward travel.
export function Pond({ children }: { children: ReactNode }) {
  return (
    <div className="pond relative overflow-hidden rounded-[28px] border border-line px-6 pt-6 pb-20">
      <div className="relative z-10">{children}</div>
      <div aria-hidden="true" className="pointer-events-none absolute inset-x-0 bottom-0 h-[70px]">
        <svg className="reed absolute bottom-3 left-4" width="30" height="58" viewBox="0 0 30 58">
          <path d={reed} stroke="#3f7d4f" strokeWidth="2.5" fill="none" />
          <rect x="6.5" y="2" width="5" height="16" rx="2.5" fill="#7a4a32" />
          <path d="M18 58 Q20 34 17 16" stroke="#4f9461" strokeWidth="2" fill="none" />
        </svg>
        <svg className="reed absolute right-6 bottom-3" style={{ animationDelay: '-1.5s' }} width="26" height="50" viewBox="0 0 30 58">
          <path d={reed} stroke="#3f7d4f" strokeWidth="2.5" fill="none" />
          <rect x="6.5" y="2" width="5" height="16" rx="2.5" fill="#7a4a32" />
        </svg>
        <div className="swimmer flex items-end gap-1">
          <svg width="46" height="34" viewBox="0 0 64 48">
            <ellipse cx="34" cy="32" rx="22" ry="11" fill="#fdfcf7" stroke="#2f3a36" strokeOpacity=".25" />
            <circle cx="20" cy="18" r="10" fill="#1f7a63" />
            <rect x="11" y="25" width="18" height="3" rx="1.5" fill="#fff" />
            <path d="M12 15 L1 19 L12 22 Z" fill="#e3a92b" />
            <circle cx="17" cy="15" r="1.8" fill="#fff" />
            <path d="M50 28 Q60 24 58 34" fill="#fdfcf7" />
          </svg>
          <span className="mb-1 flex gap-1">{duckling}{duckling}{duckling}</span>
        </div>
        <div className="waves back absolute inset-x-0 bottom-0" />
        <div className="waves absolute inset-x-0 bottom-0" />
      </div>
    </div>
  );
}

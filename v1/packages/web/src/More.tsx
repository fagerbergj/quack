import { EXTENSIONS } from './extensions/index.ts';
import { Icon, MORE } from './Shell.tsx';

const NAVS = EXTENSIONS.flatMap((e) => e.nav ?? []);

function Line({ status }: { status: NonNullable<(typeof NAVS)[number]['status']> }) {
  const s = status();
  return s ? <span className={`-mt-1.5 text-[12.5px] ${s.warn ? 'text-warning' : 'text-muted'}`}>{s.text}</span> : null;
}

// One tile per nav entry, so an extension that registers a nav appears here with no change to this page.
export function More() {
  return (
    <div className="mx-auto max-w-3xl px-3 py-4 lg:py-8">
      <h1 className="m-0 mb-3 hidden px-1 text-[19px] lg:block">More</h1>
      <p className="m-0 mb-2 px-1 text-xs font-semibold tracking-wider text-muted uppercase">Apps</p>
      <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-3">
        {MORE.map((n) => {
          const nav = NAVS.find((x) => x.route === n.route);
          return (
            <a key={n.route} href={n.href} className="flex min-h-11 flex-col gap-2.5 rounded-[18px] border border-line bg-surface p-3.5 text-ink no-underline shadow-card hover:bg-soft">
              <span className={`grid size-[38px] place-items-center rounded-[11px] ${nav?.tint ?? 'bg-soft text-ink-2'}`}>
                <Icon route={n.route} />
              </span>
              <span className="font-display text-base font-semibold">{n.label}</span>
              {nav?.status && <Line status={nav.status} />}
            </a>
          );
        })}
      </div>
    </div>
  );
}

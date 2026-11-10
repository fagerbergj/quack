import { useMenu } from './useMenu.ts';

/** One version as the picker lists it: `short` on the button ("v2"), `name` in the menu, `status` in words and as a coloured badge. */
export type VersionItem = { id: number; short: string; name: string; status?: string; badge?: { text: string; cls: string } };

const item = 'flex min-h-11 w-full cursor-pointer items-center gap-2 border-0 bg-transparent px-3 text-left text-[13px] text-ink hover:bg-soft aria-[current=true]:font-semibold lg:min-h-9';
const chevron = <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6" /></svg>;

function Badge({ b }: { b?: VersionItem['badge'] }) {
  return b ? <span aria-hidden="true" className={`rounded px-1.5 text-[10.5px] font-semibold tracking-wide whitespace-nowrap uppercase ${b.cls}`}>{b.text}</span> : null;
}

const spoken = (v: VersionItem) => `${v.name}${v.status ? `, ${v.status}` : ''}`;

/** A compact version button and its menu of every version, newest first; `none` offers no version at all as the first entry. */
export function VersionMenu({ name, items, value, none, title, onPick }: { name: string; items: VersionItem[]; value?: number; none?: string; title?: string; onPick: (id?: number) => void }) {
  const menu = useMenu();
  const v = items.find((x) => x.id === value);
  const choose = (n?: number) => { menu.close(); onPick(n); };
  return (
    <div ref={menu.root} onKeyDown={menu.keys} className="relative">
      <button ref={menu.button} type="button" aria-haspopup="menu" aria-expanded={menu.open} aria-label={`${name}: ${v ? spoken(v) : none}`} title={title} onClick={menu.toggle} className="flex h-11 cursor-pointer items-center gap-1.5 rounded-full border border-line bg-soft px-3 text-[13px] text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link">
        <span className="font-mono font-semibold whitespace-nowrap">{v ? v.short : 'none'}</span><Badge b={v?.badge} />{chevron}
      </button>
      {menu.open && (
        <div role="menu" aria-label={name} className="absolute left-0 z-20 mt-1 min-w-60 rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          {none && <button type="button" role="menuitem" aria-current={!v || undefined} tabIndex={-1} onClick={() => choose(undefined)} className={item}>{none}</button>}
          {[...items].reverse().map((x) => (
            <button key={x.id} type="button" role="menuitem" aria-current={x === v || undefined} tabIndex={-1} onClick={() => choose(x.id)} className={item}>
              <span className="flex-1">{x.name}</span><Badge b={x.badge} /><span className="sr-only">{x.status && `, ${x.status}`}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/** Shown once the person picked a version: goes back to following the newest one as it seals. */
export function FollowNewest({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" aria-label="Follow the newest version" title="Follow the newest version" onClick={onClick} className="grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:bg-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5" /></svg>
    </button>
  );
}

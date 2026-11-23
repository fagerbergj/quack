import { useEffect, useRef, useState, type ReactNode } from 'react';
import { checkFailed, type Verdict } from './api.ts';
import { useMenu } from './useMenu.ts';

/** Where a version stands, as one mark: passed or approved, failed or sent back, waiting on the person, a draft, or still being checked. */
export type Mark = 'ok' | 'bad' | 'you' | 'draft' | 'checking';
/** One version as the picker lists it: `short` ("v2") and its mark on screen; `status` is the words, spoken and shown as the tooltip only. */
export type VersionItem = { id: number; short: string; status?: string; mark?: Mark };

export const iconButton = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:bg-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link disabled:cursor-default disabled:opacity-40';
const glyph = (d: string) => <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d} /></svg>;
const MARKS: Record<Mark, [string, ReactNode]> = {
  ok: ['bg-success-bg text-success', glyph('M5 12.5l4.5 4.5L19 7.5')],
  bad: ['bg-danger-bg text-danger', glyph('M6 6l12 12M18 6L6 18')],
  you: ['bg-warning-bg text-warning', glyph('M12 6v7M12 18h.01')],
  draft: ['border-[1.5px] border-dashed border-muted', null],
  checking: ['bg-line text-muted', glyph('M5 12h.01M12 12h.01M19 12h.01')],
};

export function VersionMark({ mark }: { mark?: Mark }) {
  if (!mark) return null;
  const [cls, g] = MARKS[mark];
  return <span aria-hidden="true" className={`grid h-5 w-5 flex-none place-items-center rounded-full ${cls}`}>{g}</span>;
}

/** A sealed version's mark and words from its verdict: over REST it has none, and null means its checks are still running. */
export function verdictMark(v: Verdict | null | undefined): Pick<VersionItem, 'mark' | 'status'> {
  if (v === undefined) return {};
  if (v === null) return { mark: 'checking', status: 'checking' };
  const fix = v.checks.filter(checkFailed).length;
  return v.passed ? { mark: 'ok', status: 'passed checks' } : { mark: 'bad', status: `failed checks${fix ? `, ${fix} to fix` : ''}` };
}

const spoken = (v: VersionItem) => `${v.short}${v.status ? `, ${v.status}` : ''}`;
const RECENT = 3;
const item = 'flex min-h-11 w-full cursor-pointer items-center gap-2.5 border-0 bg-transparent px-3 text-left text-[13px] text-ink hover:bg-soft aria-[current=true]:bg-soft lg:min-h-9';
const chevron = <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="text-muted"><path d="M6 9l6 6 6-6" /></svg>;

function Rows({ list, value, all, choose, older }: { list: VersionItem[]; value?: number; all: boolean; choose: (id: number) => void; older: () => void }) {
  return (
    <>
      {(all ? list : list.slice(0, RECENT)).map((x) => (
        <button key={x.id} type="button" role="menuitem" aria-current={x.id === value || undefined} aria-label={spoken(x)} title={x.status} tabIndex={-1} onClick={() => choose(x.id)} className={item}>
          <span className="font-mono font-semibold">{x.short}</span><VersionMark mark={x.mark} />
        </button>
      ))}
      {!all && list.length > RECENT && <button type="button" role="menuitem" tabIndex={-1} onClick={older} className={`${item} text-muted`}>Older…</button>}
    </>
  );
}

/** A version button and its menu, newest first: the latest three, then Older… for the rest. With `compare`, Compare with… lists the versions
 * older than the shown one to compare it against; `since` draws the button as the compare chip ("since v2"). */
export function VersionMenu({ name, items, value, since, compare, onPick }: { name: string; items: VersionItem[]; value?: number; since?: boolean; compare?: (id: number) => void; onPick: (id: number) => void }) {
  const menu = useMenu();
  const [all, setAll] = useState(false);
  const [comparing, setComparing] = useState(false);
  const aim = useRef<number>(undefined);
  const v = items.find((x) => x.id === value);
  const newest = items.toReversed();
  const older = v ? newest.slice(newest.indexOf(v) + 1) : [];
  // Older… and Compare with… swap the list in place, so focus moves to the first item that appeared.
  useEffect(() => {
    if (aim.current !== undefined) menu.root.current?.querySelectorAll<HTMLElement>('[role="menuitem"]')[aim.current]?.focus();
    aim.current = undefined;
  }, [all, comparing, menu.root]);
  useEffect(() => {
    if (menu.open) return;
    setAll(false);
    setComparing(false);
  }, [menu.open]);
  const choose = (n: number) => { menu.close(); (comparing ? compare! : onPick)(n); };
  const label = comparing && v ? `Compare ${v.short} with` : name;
  return (
    <div ref={menu.root} onKeyDown={menu.keys} className="relative">
      <button ref={menu.button} type="button" aria-haspopup="menu" aria-expanded={menu.open} aria-label={`${name}: ${v ? spoken(v) : 'none'}`} title={v && spoken(v)} onClick={menu.toggle} className={`flex min-h-11 cursor-pointer items-center gap-1.5 text-[13px] text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link lg:min-h-9 ${since ? 'rounded-full border-0 bg-transparent pr-1 pl-3 text-ink-2 hover:text-ink' : 'rounded-full border border-line bg-soft pr-2 pl-3'}`}>
        {since ? <span>since {v?.short}</span> : <><span className="font-mono font-semibold">{v?.short}</span><VersionMark mark={v?.mark} /></>}{chevron}
      </button>
      {menu.open && (
        <div role="menu" aria-label={label} className="absolute left-0 z-20 mt-1 max-h-[60vh] min-w-48 overflow-y-auto rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          {comparing && <p aria-hidden="true" className="m-0 px-3 pt-1.5 pb-0.5 text-[12px] text-muted">{label}</p>}
          <Rows list={comparing ? older : newest} value={value} all={all} choose={choose} older={() => { aim.current = RECENT; setAll(true); }} />
          {compare && !comparing && older.length > 0 && (
            <>
              <div role="separator" className="my-1 border-t border-line" />
              <button type="button" role="menuitem" tabIndex={-1} onClick={() => { aim.current = 0; setAll(false); setComparing(true); }} className={`${item} text-link`}>Compare with…</button>
            </>
          )}
        </div>
      )}
    </div>
  );
}

/** Shown once the person picked a version: goes back to following the newest one as it seals. */
export function FollowNewest({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" aria-label="Follow the newest version" title="Follow the newest version" onClick={onClick} className={iconButton}>
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5" /></svg>
    </button>
  );
}

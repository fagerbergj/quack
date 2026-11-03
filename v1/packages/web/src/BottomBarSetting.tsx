import { useState } from 'react';
import { MAX_PICKS, PAGES, saveBar, storedBar } from './bottomBar.ts';

const row = 'flex min-h-12 items-center gap-3 border-t border-line pl-3.5 pr-1.5 text-[15px]';
const move = 'grid size-11 flex-none cursor-pointer place-items-center rounded-lg border-0 bg-transparent text-ink-2 hover:bg-soft disabled:cursor-default disabled:opacity-30';
const fixed = 'flex min-h-12 items-center gap-3 px-3.5 text-[15px] text-muted';

export function BottomBarSetting() {
  const [picked, setPicked] = useState(storedBar);
  const set = (next: string[]) => {
    setPicked(next);
    saveBar(next);
  };
  const shift = (i: number, by: number) => {
    const next = [...picked];
    [next[i], next[i + by]] = [next[i + by]!, next[i]!];
    set(next);
  };
  const label = (route: string) => PAGES.find((p) => p.route === route)!.label;
  const full = picked.length >= MAX_PICKS;
  return (
    <fieldset className="m-0 min-w-0 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
      <legend className="px-1 text-xs font-semibold tracking-wider text-muted uppercase">Navigation</legend>
      <p className="m-0 mb-2 text-[13.5px] text-ink-2">Pick up to three for the sidebar on desktop and the bottom bar on a phone. Home and More are always there.</p>
      <div className="overflow-hidden rounded-xl border border-line">
        <div className={fixed}><span className="flex-1">Home</span><span className="text-[12.5px]">Always first</span></div>
        {picked.map((route, i) => (
          <div key={route} className={row}>
            <label className="flex min-h-11 flex-1 cursor-pointer items-center gap-3">
              <input type="checkbox" checked onChange={() => set(picked.filter((r) => r !== route))} className="size-5 accent-accent" />
              {label(route)}
            </label>
            <button type="button" aria-label={`Move ${label(route)} up`} disabled={i === 0} onClick={() => shift(i, -1)} className={move}>↑</button>
            <button type="button" aria-label={`Move ${label(route)} down`} disabled={i === picked.length - 1} onClick={() => shift(i, 1)} className={move}>↓</button>
          </div>
        ))}
        {PAGES.filter((p) => !picked.includes(p.route)).map((p) => (
          <div key={p.route} className={row}>
            <label className={`flex min-h-11 flex-1 items-center gap-3 ${full ? 'cursor-default text-muted' : 'cursor-pointer'}`}>
              <input type="checkbox" checked={false} disabled={full} onChange={() => set([...picked, p.route])} className="size-5 accent-accent" />
              {p.label}
            </label>
          </div>
        ))}
        <div className={`${fixed} border-t border-line`}><span className="flex-1">More</span><span className="text-[12.5px]">Always last</span></div>
      </div>
    </fieldset>
  );
}

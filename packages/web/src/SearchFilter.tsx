import { escapeTerm } from '@quack/sdk/query';
import { useEffect, useId, useRef, type KeyboardEvent, type RefObject } from 'react';
import { useMenu } from './useMenu.ts';

/** One filter a list page declares: '' is its default, labelled `any`. */
export type FilterField = { id: string; label: string; any: string; options: Array<{ value: string; label: string }> };
export type FilterValues = Record<string, string>;

/** The search box as q terms: each word a prefix, so the list narrows as you type; `alt` names a field the word may match instead. */
export function searchTerms(text: string, alt?: (escaped: string) => string): string[] {
  return text.split(/\s+/).filter((w) => /[\p{L}\p{N}]/u.test(w)).map((w) => (alt ? `(${escapeTerm(w)}* OR ${alt(escapeTerm(w))})` : `${escapeTerm(w)}*`));
}

const HEIGHT = 'h-10 lg:h-[34px]';
const iconBtn = `relative grid ${HEIGHT} w-10 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:bg-soft hover:text-ink lg:w-[34px]`;
const select = 'h-10 w-full min-w-0 cursor-pointer rounded-lg border border-line bg-soft px-2 text-[13px] text-ink outline-none focus:shadow-ring lg:h-[34px]';
const typing = (t: EventTarget | null) => t instanceof HTMLElement && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));

// `/` reaches the page's own search only: the sidebar's list sits outside main, and a key another handler took stays taken.
function useSlash(search: RefObject<HTMLInputElement | null>) {
  useEffect(() => {
    const key = (e: globalThis.KeyboardEvent) => {
      const el = search.current;
      if (e.key !== '/' || e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || typing(e.target) || !el?.closest('main')) return;
      e.preventDefault();
      el.focus();
    };
    addEventListener('keydown', key);
    return () => removeEventListener('keydown', key);
  }, []);
}

const Funnel = () => <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 5h18l-7 8.5V19l-4 2v-7.5z" /></svg>;
const Cross = ({ size = 14 }: { size?: number }) => <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>;

type Props = { label: string; text: string; onText: (text: string) => void; fields: FilterField[]; values: FilterValues; onValues: (v: FilterValues) => void };

// The one search-and-filter row for list pages: a search the row's width, a funnel opening the page's fields, and a chip per set filter.
export function SearchFilter({ label, text, onText, fields, values, onValues }: Props) {
  const menu = useMenu();
  const search = useRef<HTMLInputElement>(null);
  const panel = useId();
  useSlash(search);
  const active = fields.flatMap((f) => (values[f.id] ? [{ f, label: f.options.find((o) => o.value === values[f.id])?.label ?? values[f.id]! }] : []));
  const set = (id: string, v: string) => onValues({ ...values, [id]: v });
  const esc = (e: KeyboardEvent) => void (e.key === 'Escape' && menu.open && (e.stopPropagation(), menu.close()));
  return (
    <div className="flex flex-col gap-2">
      <div ref={menu.root} onKeyDown={esc} className="relative flex items-center gap-1.5">
        <label className={`flex ${HEIGHT} min-w-0 flex-1 items-center gap-2 rounded-full bg-soft px-3 text-sm text-muted focus-within:bg-surface focus-within:shadow-ring lg:text-[13px]`}>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true" className="flex-none"><circle cx="11" cy="11" r="7" /><path d="M20 20l-3.5-3.5" /></svg>
          <span className="sr-only">{label}</span>
          <input ref={search} type="search" value={text} onChange={(e) => onText(e.target.value)} placeholder={label} className="min-w-0 flex-1 border-0 bg-transparent text-ink outline-none" />
        </label>
        <button ref={menu.button} type="button" aria-label={active.length ? `Filter, ${active.length} set` : 'Filter'} title="Filter" aria-haspopup="dialog" aria-expanded={menu.open} aria-controls={panel} onClick={menu.toggle} className={`${iconBtn} ${active.length ? 'text-link' : ''}`}>
          <Funnel />
          {active.length > 0 && <span aria-hidden="true" className="absolute -top-0.5 -right-0.5 grid h-4 min-w-4 place-items-center rounded-full bg-link px-1 text-[10px] font-semibold text-surface">{active.length}</span>}
        </button>
        {/* On a phone the shell's column is a stacking context under the tab bar, so the sheet sits on the bar, not behind it. */}
        {menu.open && (
          <section id={panel} role="dialog" aria-label="Filters" className="fixed inset-x-0 bottom-[calc(48px+var(--safe-b))] z-50 flex flex-col gap-3 rounded-t-[20px] border-t border-line bg-canvas p-4 shadow-lift lg:absolute lg:inset-x-auto lg:top-full lg:right-0 lg:bottom-auto lg:mt-1 lg:w-72 lg:rounded-xl lg:border lg:border-line-strong lg:bg-surface lg:p-3 lg:shadow-float">
            <div className="flex items-center justify-between">
              <h2 className="m-0 text-[15px] font-semibold lg:text-[13px]">Filters</h2>
              <button type="button" aria-label="Close filters" title="Close" onClick={() => menu.close()} className={iconBtn}><Cross size={18} /></button>
            </div>
            {fields.map((f, i) => (
              <div key={f.id} className="flex flex-col gap-1">
                <label htmlFor={`${panel}-${f.id}`} className="text-[12px] font-medium text-muted">{f.label}</label>
                <select id={`${panel}-${f.id}`} autoFocus={i === 0} value={values[f.id] ?? ''} onChange={(e) => set(f.id, e.target.value)} className={select}>
                  <option value="">{f.any}</option>
                  {f.options.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                </select>
              </div>
            ))}
          </section>
        )}
      </div>
      {menu.open && <div aria-hidden="true" className="fixed inset-0 z-40 bg-ink/25 lg:hidden" />}
      {active.length > 0 && (
        <ul aria-label="Set filters" className="m-0 flex list-none flex-wrap gap-1.5 p-0">
          {active.map(({ f, label: shown }) => (
            <li key={f.id}>
              <button type="button" aria-label={`Remove ${f.label}: ${shown}`} title={`Remove ${f.label}: ${shown}`} onClick={() => set(f.id, '')} className={`flex ${HEIGHT} cursor-pointer items-center gap-1.5 rounded-full border border-link bg-link-bg px-3 text-[13px] text-link`}>
                <span className="text-link/75">{f.label}:</span> {shown}
                <Cross />
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

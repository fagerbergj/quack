import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { Dialog } from '../../Dialog.tsx';
import { useMedia } from '../../useDesktop.ts';
import type { Mark } from './diff.ts';
import { CommentButton, CommentsRow } from './Comments.tsx';
import { Inline, Reasons } from './Info.tsx';
import { lede, saysMore, shortChange, type Item } from './review.ts';
import { BeforeAfter, label } from './Sections.tsx';
import { factText, listedBreaks, since, type ContractSpec, type Fact } from './spec.ts';

// `look`: the jump came from the spec, so the diagram scrolls into view as well as centring.
type Ping = { ptr: string; n: number; look?: boolean };
/** The changes on screen and where the reader is among them: `flash` is the element just jumped to, `aim` where the diagram should centre.
 * `compared` when the page compares two versions, and shows changes only from that comparison. */
export type Review = { items: Item[]; base?: string; now?: string; compared?: boolean; at: number; flash?: Ping; aim?: Ping; go: (i: number) => void; show: (ptr: string) => void; centre?: (ptr: string) => void };

export const ReviewContext = createContext<Review>({ items: [], at: -1, go: () => {}, show: () => {} });
export const useReview = () => useContext(ReviewContext);

/** Next change and the jumps from the diagram, as state: each jump is numbered so a second jump to the same element scrolls again. */
export function useStepper(items: Item[], base?: string, now?: string, compared?: boolean): Review {
  const [at, setAt] = useState(-1);
  const [flash, setFlash] = useState<Ping>();
  const [aim, setAim] = useState<Ping>();
  const n = useRef(0);
  useEffect(() => setAt(-1), [items]);
  const show = useCallback((ptr: string) => setFlash({ ptr, n: ++n.current }), []);
  const centre = useCallback((ptr: string) => setAim({ ptr, n: ++n.current, look: true }), []);
  const go = useCallback((i: number) => {
    setAt(i);
    const it = items[i];
    if (!it) return setFlash(undefined);
    setFlash({ ptr: it.target, n: ++n.current });
    setAim({ ptr: it.target, n: n.current });
  }, [items]);
  return useMemo(() => ({ items, base, now, compared, at, flash, aim, go, show, centre }), [items, base, now, compared, at, flash, aim, go, show, centre]);
}

export const FLASH = 'bg-link-bg shadow-[inset_3px_0_0_var(--color-link)]';

/** An element the reader can be sent to: when it is the one jumped to, it scrolls into view, takes focus and is highlighted. */
export function useFlash<T extends HTMLElement>(ptr: string | undefined) {
  const { flash } = useReview();
  const ref = useRef<T>(null);
  const on = !!ptr && flash?.ptr === ptr;
  // scrollIntoView ignores the CSS reduced-motion rule when asked for smooth, so the choice is made here.
  const calm = useMedia('(prefers-reduced-motion: reduce)');
  useEffect(() => {
    if (!on) return;
    ref.current?.scrollIntoView?.({ block: 'center', behavior: calm ? 'auto' : 'smooth' });
    ref.current?.focus({ preventScroll: true });
  }, [on, flash?.n]);
  return { ref, on, cls: on ? FLASH : '' };
}

/** The change at exactly this pointer, if the comparison has one. */
export const useItem = (ptr: string) => useReview().items.find((i) => i.ptr === ptr);

/** A change mark: a dot for new or changed, a diamond for a change that breaks a caller, so shape tells them apart as well as colour. */
export function Glyph({ m }: { m?: Mark | 'removed' }) {
  if (!m) return null;
  return m === 'breaking'
    ? <span aria-hidden="true" className="inline-block h-2 w-2 flex-none rotate-45 bg-danger" />
    : <span aria-hidden="true" className="inline-block h-2.5 w-2.5 flex-none rounded-full border border-warning bg-mark" />;
}

const more = 'cursor-pointer border-0 bg-transparent p-0 text-[length:inherit] text-link underline underline-offset-2 focus-ring';

/** A description by its first sentence, the rest one press away; with no clean first sentence, only the press. */
// `extra` opens with the rest; `bare` names the button when there is no first sentence to show beside it.
export function Desc({ text, name = 'More', bare = 'Description', extra }: { text: string; name?: string; bare?: string; extra?: ReactNode }) {
  const [open, setOpen] = useState(false);
  const short = lede(text);
  const rest = saysMore(text, short) || !!extra;
  return (
    <>
      <span className="whitespace-pre-line">
        <Inline text={open || !rest ? text : short} />
        {rest && <>{short && ' '}<button type="button" aria-expanded={open} onClick={() => setOpen(!open)} className={more}>{open ? 'Less' : short ? name : bare}</button></>}
      </span>
      {open && extra}
    </>
  );
}

// A short value reads on one line; prose reads as its first changed sentence on each side.
function Sides({ was, now, base, at }: { was: string; now: string; base: string; at: string }) {
  const s = was.length < 60 && now.length < 60 ? { was, now, more: false } : shortChange(was, now);
  const side = (v: string, t: string, cls: string) => t && <div className="flex gap-2"><dt className="w-12 flex-none font-mono text-[12px] text-muted">{v}</dt><dd className={`m-0 min-w-0 [overflow-wrap:anywhere] ${cls}`}><Inline text={t} /></dd></div>;
  return (
    <dl className="m-0 flex flex-col gap-1">
      {side(base, s.was, 'text-muted line-through decoration-danger/60')}
      {side(at, s.now, 'text-ink')}
      {!s.was && !s.now && <div className="text-muted">Only spacing or punctuation changed.</div>}
    </dl>
  );
}

/** What changed at one element, short: before and after per changed part, the exact wording one press away, and the callers a break names. */
export function ShortChange({ item }: { item: Item }) {
  const { base = 'before', now = 'now' } = useReview();
  const [exact, setExact] = useState(false);
  const rows = item.mark === 'changed' ? item.rows : [];
  if (!rows.length && !item.callers.length) return null;
  return (
    <div className="flex basis-full flex-col gap-2 rounded-lg border border-line bg-surface px-3 py-2 text-[13px]">
      {rows.map((r) => (
        <div key={r.name} className="flex flex-col gap-1">
          {rows.length > 1 && <span className={label}>{r.name}</span>}
          {exact ? <BeforeAfter was={r.was} now={r.now} words /> : <Sides was={r.was} now={r.now} base={base} at={now} />}
        </div>
      ))}
      {rows.length > 0 && <button type="button" aria-expanded={exact} onClick={() => setExact(!exact)} className={`${more} self-start`}>{exact ? 'Short form' : 'Exact wording'}</button>}
      {item.callers.length > 0 && <p className="m-0 rounded-md bg-danger-bg px-2 py-1.5 text-danger"><strong>Breaks:</strong> {item.callers.join('; ')}</p>}
    </div>
  );
}

const sectionTitle = 'm-0 text-[17px] font-semibold';

/** One line per change; pressing one goes to it in the diagram and the spec. */
function ItemList() {
  const { items, at, go } = useReview();
  return (
    <ol className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-xl border border-line bg-surface p-0">
      {items.map((it, i) => (
        <li key={`${it.ptr}${it.line}`}>
          <button type="button" aria-current={at === i ? 'true' : undefined} onClick={() => go(i)} className="flex min-h-11 w-full cursor-pointer items-center gap-2.5 border-0 bg-transparent px-3 py-2 text-left text-[14px] text-ink hover:bg-soft aria-[current=true]:bg-link-bg focus-ring">
            <Glyph m={it.breaking ? 'breaking' : it.mark} />
            <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">{it.line}</span>
            {it.breaking && <span className="flex-none text-[12px] font-semibold text-danger">Breaks a caller</span>}
          </button>
        </li>
      ))}
    </ol>
  );
}

const FACT_LISTS = [['guarantees', '#guarantees'], ['nonGoals', '#non-goals']] as const;
const factPtr = (spec: ContractSpec, id: string) => FACT_LISTS.find(([k]) => spec.intent[k]?.some((f) => typeof f !== 'string' && f.id === id))?.[1];

// The fact ids a change row names, each a link to its fact; a removed id has nothing left to link to.
function FactLinks({ spec, ids }: { spec: ContractSpec; ids: string[] }) {
  const { show } = useReview();
  return (
    <span className="flex flex-wrap items-center gap-x-2 text-[12px]">
      <span className={label}>Facts</span>
      {ids.map((id) => {
        const at = factPtr(spec, id);
        return at ? <button key={id} type="button" onClick={() => show(`${at}/${id}`)} className="min-h-6 cursor-pointer border-0 bg-transparent p-0 font-mono text-link underline underline-offset-2 focus-ring">{id}</button> : <code key={id} className="font-mono text-muted">{id}</code>;
      })}
    </span>
  );
}

// The agent's own account of each change, one line each; was, now and why on demand.
function Told({ spec }: { spec: ContractSpec }) {
  return (
    <ul className="m-0 flex list-none flex-col gap-2 p-0 text-[14px]">
      {(spec.changes ?? []).map((c) => (
        <li key={c.choice} className="group/c flex flex-col gap-1 [overflow-wrap:anywhere]">
          <span className="flex items-center gap-1"><code className="min-w-0 flex-1 font-mono text-[12px] text-muted">{c.choice}</code><CommentButton target={{ anchor: { on: 'change', choice: c.choice }, tag: 'Change', name: c.choice, text: lede(c.now) || c.now }} /></span>
          {!!c.facts?.length && <FactLinks spec={spec} ids={c.facts} />}
          <Desc text={c.now} bare="Read the change" extra={<span className="flex flex-col gap-1 text-[13px] text-ink-2">{c.was && <span><span className={label}>Before </span><Inline text={c.was} /></span>}<span><span className={label}>Why </span><Inline text={c.why} /></span></span>} />
        </li>
      ))}
    </ul>
  );
}

function Breaks({ spec }: { spec: ContractSpec }) {
  return (
    <ul className="m-0 flex list-none flex-col gap-2 p-0 text-[14px]">
      {listedBreaks(spec).map((b, i) => (
        <li key={i} className="flex gap-2 [overflow-wrap:anywhere]">
          <span className="mt-1.5"><Glyph m="breaking" /></span>
          <span className="flex min-w-0 flex-col gap-0.5">
            <Desc text={b.what} bare="Read the break" extra={b.migration && <span className="text-[13px] text-ink-2"><span className={label}>Migration </span><Inline text={b.migration} /></span>} />
            {b.callers.length > 0 && <span className="text-[13px] text-muted">Callers: {b.callers.join('; ')}</span>}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** The changes first: the compared versions' changes, the agent's account of them, what could break and what was checked safe. */
export function Changes({ spec }: { spec: ContractSpec }) {
  const { items, base, compared } = useReview();
  const id = useId();
  const told = spec.changes?.length ?? 0, breaks = listedBreaks(spec).length, safe = spec.compatibility?.safe ?? [];
  const flash = useFlash<HTMLElement>('#changes');
  // Beside its conversation a contract shows changes only between the versions it compares; on its own, the spec's account of them.
  if (compared ? !items.length : !told && !breaks) return null;
  const h3 = `m-0 font-sans ${label}`;
  return (
    <section ref={flash.ref} tabIndex={-1} aria-labelledby={id} data-ptr="#changes" className="flex scroll-mt-16 flex-col gap-3 outline-none">
      <h2 id={id} className={sectionTitle}>Changes against {items.length ? base : since(spec)}</h2>
      {items.length > 0 && <ItemList />}
      {told > 0 && <>{items.length > 0 && <h3 className={h3}>As the agent describes them, against {since(spec)}</h3>}<Told spec={spec} /></>}
      {breaks > 0 && <><h3 className={h3}>What could break</h3><Breaks spec={spec} /></>}
      {safe.length > 0 && <><h3 className={h3}>Checked safe</h3><ul className="m-0 flex list-none flex-col gap-1.5 p-0 text-[14px] [overflow-wrap:anywhere]">{safe.map((s) => <li key={s}><Inline text={s} /></li>)}</ul></>}
    </section>
  );
}

/** What the contract is for, in one sentence; the full purpose one press away. */
export function Purpose({ spec }: { spec: ContractSpec }) {
  const item = useReview().items.find((i) => i.target === '#purpose');
  const flash = useFlash<HTMLElement>('#purpose');
  return (
    <section ref={flash.ref} tabIndex={-1} aria-label="What it does" data-ptr="#purpose" className={`flex flex-col gap-2 rounded-lg text-[15px] leading-relaxed text-ink-2 outline-none ${flash.cls}`}>
      <p className="m-0 flex items-baseline gap-2 [overflow-wrap:anywhere]">{item && <Glyph m="changed" />}<Desc text={spec.intent.purpose} name="Full purpose" /></p>
      {item && <ShortChange item={item} />}
    </section>
  );
}

const close = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:bg-soft focus-visible:outline-2 focus-visible:outline-link';

/** A bottom sheet on a phone, a centred panel on a wider screen; `actions` sit in its footer. */
export function Sheet({ title, onClose, children, actions }: { title: string; onClose: () => void; children: ReactNode; actions?: ReactNode }) {
  const id = useId();
  return (
    <Dialog labelledBy={id} onClose={onClose} className="fixed inset-x-0 bottom-0 z-50 flex max-h-[88dvh] flex-col rounded-t-[20px] border-t border-line bg-canvas text-ink shadow-lift outline-none sm:inset-x-auto sm:top-1/2 sm:bottom-auto sm:left-1/2 sm:w-[min(600px,92vw)] sm:-translate-x-1/2 sm:-translate-y-1/2 sm:rounded-[20px] sm:border">
      <header className="flex items-center gap-2 py-2 pr-2 pl-4">
        <h2 id={id} className="m-0 min-w-0 flex-1 text-[17px] font-semibold [overflow-wrap:anywhere]">{title}</h2>
        <button type="button" aria-label="Close" onClick={onClose} className={close}><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg></button>
      </header>
      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-4 pb-4 text-[14px] leading-relaxed">{children}</div>
      {actions && <div className="flex justify-end gap-2 border-t border-line px-4 pt-3 pb-[max(0.75rem,var(--safe-b))]">{actions}</div>}
    </Dialog>
  );
}

const ul = 'm-0 flex list-disc flex-col gap-2 pl-5 [overflow-wrap:anywhere]';
const List = ({ items }: { items: string[] }) => <ul className={ul}>{items.map((s) => <li key={s}><Inline text={s} /></li>)}</ul>;

// A fact's text with its id as quiet secondary text; a change row's link to it lands here.
function FactItem({ f, ptr }: { f: Fact; ptr: string }) {
  const id = typeof f === 'string' ? undefined : f.id;
  const flash = useFlash<HTMLLIElement>(id ? `${ptr}/${id}` : undefined);
  const text = factText(f);
  const on = ptr === '#guarantees' ? 'guarantee' : 'nonGoal';
  return (
    <li ref={flash.ref} tabIndex={id ? -1 : undefined} className={`group/c rounded outline-none ${flash.cls}`}>
      <Inline text={text} />{id && <> <code className="font-mono text-[12px] text-muted">{id}</code></>}
      <CommentButton target={{ anchor: { on, text, id }, tag: on === 'guarantee' ? 'Guarantee' : 'Out of scope', name: id ?? lede(text), text }} />
    </li>
  );
}

const prefixOf = (f: Fact) => (typeof f === 'string' || !f.id.includes('.') ? '' : f.id.slice(0, f.id.indexOf('.')));

/** Guarantees or non-goals, grouped by their ids' dotted prefix when the list has more than one. */
function Facts({ items, ptr }: { items: Fact[]; ptr: string }) {
  const list = (fs: Fact[]) => <ul className={ul}>{fs.map((f, i) => <FactItem key={i} f={f} ptr={ptr} />)}</ul>;
  const groups = [...Map.groupBy(items, prefixOf)];
  if (groups.length < 2) return list(items);
  return <>{groups.map(([p, fs]) => <section key={p} aria-label={p || undefined} className="flex flex-col gap-1.5">{p && <h3 className={`m-0 font-mono ${label}`}>{p}</h3>}{list(fs)}</section>)}</>;
}

type Part = { ptr: string; title: string; count?: number; body: ReactNode };

// A decision with its reasons, and a comment on it by its choice id.
function Decision({ c }: { c: ContractSpec['choices'][number] }) {
  return <div className="group/c flex items-start gap-1"><div className="min-w-0 flex-1"><Reasons c={c} /></div><CommentButton target={{ anchor: { on: 'choice', id: c.id }, tag: 'Decision', name: c.id, text: c.decision }} /></div>;
}

function parts(spec: ContractSpec, extra: Part[]): Part[] {
  const i = spec.intent;
  const who = [i.audience && <p key="a" className="m-0"><Inline text={i.audience} /></p>, !!i.consumers?.length && <List key="c" items={i.consumers} />];
  return [
    ...(i.guarantees?.length ? [{ ptr: '#guarantees', title: 'Guarantees', count: i.guarantees.length, body: <Facts items={i.guarantees} ptr="#guarantees" /> }] : []),
    ...(i.nonGoals?.length ? [{ ptr: '#non-goals', title: 'Out of scope', count: i.nonGoals.length, body: <Facts items={i.nonGoals} ptr="#non-goals" /> }] : []),
    ...(i.audience || i.consumers?.length ? [{ ptr: '#audience', title: 'Who it is for', body: <>{who}</> }] : []),
    ...(spec.choices.length ? [{ ptr: '#decisions', title: 'Decisions', count: spec.choices.length, body: spec.choices.map((c) => <Decision key={c.id} c={c} />) }] : []),
    ...extra,
  ];
}

function PartButton({ p, onOpen }: { p: Part; onOpen: () => void }) {
  const items = useReview().items.filter((i) => i.target === p.ptr);
  const flash = useFlash<HTMLButtonElement>(p.ptr);
  return (
    <li>
      <button ref={flash.ref} type="button" data-ptr={p.ptr} onClick={onOpen} className={`flex min-h-11 w-full cursor-pointer items-center gap-2.5 border-0 bg-transparent px-3 text-left text-[14px] text-ink hover:bg-soft focus-ring ${flash.cls}`}>
        <span className="min-w-0 flex-1">{p.title}</span>
        {items.length > 0 && <span className="flex items-center gap-1.5 text-[12px] font-semibold text-warning"><Glyph m="changed" />changed</span>}
        {p.count !== undefined && <span className="rounded-full bg-soft px-2 text-[12px] font-semibold text-ink-2 tabular-nums">{p.count}</span>}
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true" className="text-muted"><path d="M9 6l6 6-6 6" /></svg>
      </button>
    </li>
  );
}

/** The contract's long prose, as it is stored, one press away each: guarantees, out of scope, who it is for, the decisions, and any `extra`. */
export function MoreAbout({ spec, extra = [] }: { spec: ContractSpec; extra?: Part[] }) {
  const [open, setOpen] = useState<string>();
  const { items, flash } = useReview();
  const id = useId();
  const list = parts(spec, extra);
  // A link to one fact opens the part that holds it.
  useEffect(() => { const hit = flash && list.find((p) => flash.ptr.startsWith(`${p.ptr}/`)); if (hit) setOpen(hit.ptr); }, [flash]);
  const shown = list.find((p) => p.ptr === open);
  const changed = shown && items.find((i) => i.target === shown.ptr);
  if (!list.length) return null;
  return (
    <section aria-labelledby={id} className="flex flex-col gap-2">
      <h2 id={id} className={sectionTitle}>More about this contract</h2>
      <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-xl border border-line bg-surface p-0">
        {list.map((p) => <PartButton key={p.ptr} p={p} onOpen={() => setOpen(p.ptr)} />)}
        <CommentsRow />
      </ul>
      {shown && <Sheet title={shown.title} onClose={() => setOpen(undefined)}>{changed && <ShortChange item={changed} />}{shown.body}</Sheet>}
    </section>
  );
}

/** Where the reader is among the changes, and the one control that steps through them in the diagram and the spec together. */
export function Stepper({ onTop }: { onTop: () => void }) {
  const { items, base, at, go } = useReview();
  const n = items.length;
  const last = at >= n - 1;
  const next = () => (last ? (go(-1), onTop()) : go(at + 1));
  const upcoming = items[last ? 0 : at + 1];
  return (
    <div className="sticky top-0 z-10 -mx-4 flex min-h-14 items-center gap-3 border-b border-line bg-canvas/95 px-4 py-1.5 backdrop-blur sm:-mx-5 sm:px-5">
      <p aria-live="polite" className="m-0 min-w-0 flex-1 text-[14px] text-ink-2">{at < 0 ? <><strong className="text-ink">{n} {n === 1 ? 'change' : 'changes'}</strong> against {base}</> : <>Change <strong className="text-ink">{at + 1} of {n}</strong> against {base}</>}</p>
      <button type="button" onClick={next} className="flex min-h-11 flex-none cursor-pointer items-center gap-2 rounded-full border border-line bg-surface px-4 text-[14px] font-semibold text-ink hover:bg-soft focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link">
        {!last && <Glyph m={upcoming?.breaking ? 'breaking' : 'changed'} />}
        {at < 0 ? 'First change' : last ? 'Back to the top' : 'Next change'}
      </button>
    </div>
  );
}

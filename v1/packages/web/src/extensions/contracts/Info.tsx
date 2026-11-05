import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties } from 'react';
import { createPortal } from 'react-dom';
import type { Choice } from './spec.ts';

const SOURCE: Record<Choice['why'][number]['source'], { label: string; cls: string }> = {
  answer: { label: 'You', cls: 'text-warning' },
  guide: { label: 'Guide', cls: 'text-accent-ink' },
  code: { label: 'Existing', cls: 'text-link' },
};

// Decision and why text is Markdown-flavoured prose; only `code` spans are rendered, inline.
// One span, so a flex parent keeps the sentence and its code on one line instead of stacking each piece.
export function Inline({ text }: { text: string }) {
  return <span>{text.split(/`([^`]+)`/).map((t, i) => (i % 2 ? <code key={i} className="rounded bg-soft px-1 font-mono text-[0.92em]">{t}</code> : t))}</span>;
}

/** The choices made at one pointer of the document. */
export const choicesAt = (choices: Choice[], ptr: string) => choices.filter((c) => c.pointer === ptr);

/** Choices about the whole contract, or about a part the page does not draw; the title carries them so none is lost. */
export const unplaced = (choices: Choice[], drawn: Set<string>) => choices.filter((c) => !drawn.has(c.pointer));

function Reasons({ c }: { c: Choice }) {
  return (
    <div className="flex flex-col gap-2">
      <h3 className="m-0 text-[14.5px] font-semibold [overflow-wrap:anywhere]"><Inline text={c.decision} /></h3>
      <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
        {c.why.map((w, i) => (
          <li key={i} className="flex gap-2.5 text-[13px] leading-snug">
            <span className={`w-14 flex-none text-xs font-semibold ${SOURCE[w.source].cls}`}>{SOURCE[w.source].label}</span>
            <span className="flex min-w-0 flex-1 flex-col gap-0.5 [overflow-wrap:anywhere]">
              {w.source === 'answer' ? <><span className="text-muted">Answered “{w.ref}”</span><strong className="font-semibold"><Inline text={w.text} /></strong></> : <><Inline text={w.text} /><code className="font-mono text-xs text-muted">{w.ref}</code></>}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

const GAP = 6, EDGE = 16, WIDTH = 380;

// Below the button when it fits, else above; clamped to the viewport so a phone gets a near-full-width card.
function place(btn: HTMLElement): CSSProperties {
  const r = btn.getBoundingClientRect(), vw = innerWidth, vh = innerHeight;
  const width = Math.min(WIDTH, vw - 2 * EDGE);
  const left = Math.min(Math.max(r.right - width, EDGE), vw - width - EDGE);
  const below = vh - r.bottom - GAP - EDGE, above = r.top - GAP - EDGE;
  return below >= 220 || below >= above ? { left, width, top: r.bottom + GAP, maxHeight: below } : { left, width, bottom: vh - r.top + GAP, maxHeight: above };
}

/** An info button on the element a decision governs: hover or focus-and-press shows the decisions and their reasons. */
export function Info({ choices, label }: { choices: Choice[]; label?: string }) {
  const [open, setOpen] = useState<'hover' | 'pinned'>();
  const [style, setStyle] = useState<CSSProperties>();
  const btn = useRef<HTMLButtonElement>(null), pop = useRef<HTMLDivElement>(null), timer = useRef<number>(undefined), pressed = useRef(false);
  const id = useId();
  const name = label ?? (choices.length === 1 ? `Why: ${choices[0]!.decision}` : `Why: ${choices.length} decisions`);

  useLayoutEffect(() => {
    if (!open) return;
    const at = () => btn.current && setStyle(place(btn.current));
    at();
    addEventListener('scroll', at, true);
    addEventListener('resize', at);
    return () => (removeEventListener('scroll', at, true), removeEventListener('resize', at));
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const key = (e: KeyboardEvent) => e.key === 'Escape' && (setOpen(undefined), btn.current?.focus());
    const away = (e: PointerEvent) => !btn.current?.contains(e.target as Node) && !pop.current?.contains(e.target as Node) && setOpen(undefined);
    addEventListener('keydown', key);
    addEventListener('pointerdown', away);
    return () => (removeEventListener('keydown', key), removeEventListener('pointerdown', away));
  }, [open]);

  if (!choices.length) return null;
  const hold = () => clearTimeout(timer.current);
  const leave = () => open === 'hover' && (timer.current = window.setTimeout(() => setOpen(undefined), 150));
  return (
    <>
      <button
        ref={btn} type="button" aria-label={name} aria-expanded={!!open} aria-controls={open ? id : undefined}
        onClick={() => ((pressed.current = false), setOpen(open === 'pinned' ? undefined : 'pinned'))}
        // Keyboard focus shows the reasons the way hover does; a press focuses too, and its click decides instead.
        onPointerDown={() => void (pressed.current = true)}
        onFocus={() => (pressed.current ? (pressed.current = false) : !open && setOpen('hover'))}
        onBlur={() => ((pressed.current = false), open === 'hover' && setOpen(undefined))}
        onMouseEnter={(e) => (hold(), e.nativeEvent instanceof MouseEvent && !open && setOpen('hover'))} onMouseLeave={leave}
        className="-my-2 inline-grid min-h-11 min-w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-muted hover:text-link aria-expanded:text-link"
      >
        <span className="flex items-center gap-0.5">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><circle cx="12" cy="12" r="9.5" /><path d="M12 11v6M12 7.5v.01" /></svg>
          {choices.length > 1 && <span aria-hidden="true" className="text-[11px] font-semibold">{choices.length}</span>}
        </span>
      </button>
      {open && createPortal(
        <div ref={pop} id={id} role="dialog" aria-label={name} onMouseEnter={hold} onMouseLeave={leave} style={{ position: 'fixed', ...style }} className="z-50 flex flex-col gap-4 overflow-y-auto rounded-2xl border border-line-strong bg-surface p-4 text-ink shadow-lift">
          {choices.map((c) => <Reasons key={c.id} c={c} />)}
        </div>,
        document.body,
      )}
    </>
  );
}

import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';
const SCRIM = 'fixed inset-0 z-40 bg-ink/25 backdrop-blur-[2px]';

type Props = {
  /** The accessible name, or the id of the heading that gives it. */
  label?: string;
  labelledBy?: string;
  onClose: () => void;
  /** The panel's own classes; it is the element with role="dialog". */
  className: string;
  /** The backdrop's classes; a click on it closes. */
  scrim?: string;
  children: ReactNode;
};

// The one modal: Escape or the backdrop closes it, Tab stays inside, and focus returns to whatever had it before it opened.
// Focus starts on an element marked data-autofocus, else one that autoFocus already focused, else the first control.
export function Dialog({ label, labelledBy, onClose, className, scrim = SCRIM, children }: Props) {
  const panel = useRef<HTMLElement>(null);
  // Read while rendering, before an autoFocus child takes focus on commit.
  const [opener] = useState(() => document.activeElement as HTMLElement | null);
  useEffect(() => {
    const p = panel.current;
    if (p && !p.contains(document.activeElement)) (p.querySelector<HTMLElement>('[data-autofocus]') ?? p.querySelector<HTMLElement>(FOCUSABLE) ?? p).focus();
    return () => opener?.focus();
  }, []);
  const keys = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') return (e.stopPropagation(), onClose());
    if (e.key !== 'Tab') return;
    const f = [...(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter((el) => el.tabIndex >= 0 && !(el as HTMLButtonElement).disabled);
    const end = e.shiftKey ? f[0] : f.at(-1);
    if (!f.length || document.activeElement === end || !panel.current?.contains(document.activeElement)) {
      e.preventDefault();
      (e.shiftKey ? f.at(-1) : f[0])?.focus();
    }
  };
  // On body, so no fixed ancestor's stacking context can put the phone tab bar over it.
  return createPortal(
    <>
      <div aria-hidden="true" onClick={onClose} className={scrim} />
      <section ref={panel} role="dialog" aria-modal="true" aria-label={label} aria-labelledby={labelledBy} tabIndex={-1} onKeyDown={keys} className={className}>
        {children}
      </section>
    </>,
    document.body,
  );
}

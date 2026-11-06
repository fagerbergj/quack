import { useEffect, useRef, useState, type KeyboardEvent } from 'react';

const itemsIn = (root: HTMLElement | null) => [...(root?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
const STEP: Partial<Record<string, (at: number, n: number) => number>> = { ArrowDown: (at) => at + 1, ArrowUp: (at) => at - 1, Home: () => 0, End: (_, n) => n - 1 };

/** A menu button's popup: opening focuses its first item, the arrows, Home and End move, Escape closes back to the button, and Tab or a press outside closes it. */
export function useMenu() {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    itemsIn(root.current)[0]?.focus();
    const away = (e: PointerEvent) => void (root.current?.contains(e.target as Node) || setOpen(false));
    document.addEventListener('pointerdown', away);
    return () => document.removeEventListener('pointerdown', away);
  }, [open]);
  const close = (refocus = true) => {
    setOpen(false);
    if (refocus) button.current?.focus();
  };
  const keys = (e: KeyboardEvent) => {
    const list = itemsIn(root.current);
    const step = STEP[e.key];
    if (step && !open) return (e.preventDefault(), setOpen(true));
    if (step && list.length) return (e.preventDefault(), list[(step(list.indexOf(document.activeElement as HTMLElement), list.length) + list.length) % list.length]!.focus());
    if (e.key === 'Escape' && open) close();
    else if (e.key === 'Tab' && open) close(false);
  };
  return { open, toggle: () => setOpen(!open), close, root, button, keys };
}

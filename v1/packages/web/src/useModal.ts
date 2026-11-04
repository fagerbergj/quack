import { useEffect, useRef, type KeyboardEvent } from 'react';

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

// Esc closes, Tab stays inside the panel, and focus returns to the opener when it unmounts.
export function useModal(onClose: () => void) {
  const panel = useRef<HTMLElement>(null);
  const closeBtn = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    closeBtn.current?.focus();
    return () => opener?.focus();
  }, []);
  const keys = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') return onClose();
    if (e.key !== 'Tab') return;
    const f = [...(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter((el) => el.tabIndex >= 0);
    const end = e.shiftKey ? f[0] : f.at(-1);
    if (document.activeElement === end) {
      e.preventDefault();
      (e.shiftKey ? f.at(-1) : f[0])?.focus();
    }
  };
  return { panel, closeBtn, keys };
}

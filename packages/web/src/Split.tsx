import { useRef, useState, type CSSProperties, type KeyboardEvent, type PointerEvent } from 'react';

const KEY = 'quack.split.chat';
export const CHAT_MIN = 320;
export const PANE_MIN = 360;
export const CHAT_DEFAULT = 440;
const STEP = 24;

/** The chat column's width, kept between CHAT_MIN and what leaves the pane PANE_MIN of a row `total` wide (unknown before layout). */
export const clampChat = (w: number, total?: number) => Math.round(Math.max(CHAT_MIN, total ? Math.min(w, total - PANE_MIN) : w));

function read(): number {
  try {
    const v = Number(localStorage.getItem(KEY));
    return Number.isFinite(v) && v >= CHAT_MIN ? v : CHAT_DEFAULT;
  } catch {
    return CHAT_DEFAULT;
  }
}

/** The chat and pane split from lg up: a drag or the arrow keys on the divider resize the chat, and this browser remembers the width. */
export function useSplit() {
  const row = useRef<HTMLDivElement>(null);
  const drag = useRef<{ x: number; w: number }>(undefined);
  const [width, setWidth] = useState(read);
  const total = () => row.current?.clientWidth || undefined;
  const set = (w: number, save: boolean) => {
    const next = clampChat(w, total());
    setWidth(next);
    if (save) try { localStorage.setItem(KEY, String(next)); } catch { /* the width is a convenience */ }
  };
  const keys = (e: KeyboardEvent) => {
    const to = { ArrowLeft: width - STEP, ArrowRight: width + STEP, Home: CHAT_MIN, End: total() ?? width }[e.key];
    if (to === undefined) return;
    e.preventDefault();
    set(to, true);
  };
  const move = (e: PointerEvent, save: boolean) => void (drag.current && set(drag.current.w + e.clientX - drag.current.x, save));
  const t = total();
  const separator = {
    role: 'separator', 'aria-orientation': 'vertical', 'aria-label': 'Resize the chat', 'aria-valuemin': CHAT_MIN, 'aria-valuenow': width, 'aria-valuemax': t && t - PANE_MIN, tabIndex: 0,
    onKeyDown: keys,
    onPointerDown: (e: PointerEvent<HTMLElement>) => { e.currentTarget.setPointerCapture?.(e.pointerId); drag.current = { x: e.clientX, w: width }; },
    onPointerMove: (e: PointerEvent) => move(e, false),
    onPointerUp: (e: PointerEvent) => { move(e, true); drag.current = undefined; },
  } as const;
  return { row, style: { '--chat-w': `${width}px` } as CSSProperties, separator };
}

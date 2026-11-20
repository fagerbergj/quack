import { useLayoutEffect, useRef, useState } from 'react';

const NEAR = 48;

// Opens at the newest message and stays there while content grows (late loads, streaming) until the person scrolls up.
export function useStickToBottom() {
  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  const prevTop = useRef(0);
  const [away, setAway] = useState(false);
  const toBottom = () => {
    const el = scroller.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
    prevTop.current = el.scrollTop;
  };
  useLayoutEffect(() => {
    toBottom();
    // The scroller resizes too (the phone keyboard), which must not leave the newest message behind the composer.
    const ro = new ResizeObserver(() => pinned.current && toBottom());
    for (const el of [content.current, scroller.current]) el && ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const onScroll = () => {
    const el = scroller.current!;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR;
    const up = el.scrollTop < prevTop.current;
    prevTop.current = el.scrollTop;
    if (near) {
      pinned.current = true;
      setAway(false);
    } else if (up) {
      pinned.current = false;
      setAway(true);
    }
  };
  const jump = () => {
    pinned.current = true;
    setAway(false);
    toBottom();
  };
  return { scroller, content, away, onScroll, jump };
}

import { useLayoutEffect, useRef, useState } from 'react';

const NEAR = 48;

// Opens at the newest message and stays there while content grows (late loads, streaming) until the person scrolls up.
export function useStickToBottom() {
  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  const [away, setAway] = useState(false);
  const toBottom = () => scroller.current && (scroller.current.scrollTop = scroller.current.scrollHeight);
  useLayoutEffect(() => {
    toBottom();
    // The scroller resizes too (the phone keyboard), which must not leave the newest message behind the composer.
    const ro = new ResizeObserver(() => pinned.current && toBottom());
    for (const el of [content.current, scroller.current]) el && ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const onScroll = () => {
    const el = scroller.current!;
    pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR;
    setAway(!pinned.current);
  };
  const jump = () => {
    pinned.current = true;
    setAway(false);
    toBottom();
  };
  return { scroller, content, away, onScroll, jump };
}

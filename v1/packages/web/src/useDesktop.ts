import { useEffect, useMemo, useState } from 'react';

export const WIDE = '(min-width: 1440px)';

// Without matchMedia (tests) every query is false, the phone layout.
export function useMedia(query: string): boolean {
  const mq = useMemo(() => (typeof matchMedia === 'function' ? matchMedia(query) : undefined), [query]);
  const [on, setOn] = useState(mq?.matches ?? false);
  useEffect(() => {
    if (!mq) return;
    const update = () => setOn(mq.matches);
    mq.addEventListener('change', update);
    return () => mq.removeEventListener('change', update);
  }, [mq]);
  return on;
}

// True from lg up, where the session list lives in the sidebar.
export const useDesktop = () => useMedia('(min-width: 1024px)');

import { useEffect, useMemo, useState } from 'react';

const QUERY = '(min-width: 1024px)';

// True from lg up, where the session list lives in the sidebar. Without matchMedia (tests) it is the phone layout.
export function useDesktop(): boolean {
  const mq = useMemo(() => (typeof matchMedia === 'function' ? matchMedia(QUERY) : undefined), []);
  const [on, setOn] = useState(mq?.matches ?? false);
  useEffect(() => {
    if (!mq) return;
    const update = () => setOn(mq.matches);
    mq.addEventListener('change', update);
    return () => mq.removeEventListener('change', update);
  }, [mq]);
  return on;
}

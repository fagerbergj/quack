import { useEffect, useState } from 'react';

// iOS Safari does not shrink 100dvh for the on-screen keyboard, so a dvh-pinned pane sits under it.
export function useVisualViewportHeight(): number | undefined {
  const vv = typeof window !== 'undefined' ? window.visualViewport : null;
  const [height, setHeight] = useState<number | undefined>(vv?.height);
  useEffect(() => {
    if (!vv) return;
    const on = () => setHeight(vv.height);
    vv.addEventListener('resize', on);
    return () => vv.removeEventListener('resize', on);
  }, [vv]);
  return height;
}

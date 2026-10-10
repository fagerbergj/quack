import { useEffect, useState } from 'react'

// iOS Safari doesn't shrink `100dvh` for the on-screen keyboard, so a dvh-pinned box sits under it. This mirrors
// visualViewport.height in px for the root layout; undefined (unsupported or unchanged) leaves h-dvh in charge.
export function useVisualViewportHeight(): number | undefined {
  const vv = typeof window !== 'undefined' ? window.visualViewport : undefined
  const [height, setHeight] = useState<number | undefined>(vv?.height)
  useEffect(() => {
    if (!vv) return
    const onResize = () => setHeight(vv.height)
    vv.addEventListener('resize', onResize)
    vv.addEventListener('scroll', onResize)
    return () => {
      vv.removeEventListener('resize', onResize)
      vv.removeEventListener('scroll', onResize)
    }
  }, [vv])
  return height
}

import { useEffect, useState } from 'react'

// #1248: `100dvh` tracks the browser chrome (URL bar) but iOS Safari does not
// shrink it when the on-screen keyboard opens - the layout viewport (and dvh)
// stay full height while visualViewport.height drops, so a box pinned to
// dvh's bottom edge sits under the keyboard. This mirrors that real height in
// px so the root layout can override h-dvh with it; undefined (no support,
// or nothing has changed yet) lets the h-dvh class keep doing its job.
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

import { useState, useEffect } from 'react'

// fmtMs renders a duration in ms as a compact "1.2s" / "3m 4s" / "1h 2m 3s" string.
export function fmtMs(ms: number): string {
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  const rem = Math.floor(s % 60)
  if (m < 60) return `${m}m ${rem}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m ${rem}s`
}

// LiveTimer ticks every 100ms while running, then freezes on the final value.
export function LiveTimer({ startedAt, finishedAt }: { startedAt: number; finishedAt?: number }) {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    if (finishedAt != null) return
    const id = setInterval(() => setNow(Date.now()), 100)
    return () => clearInterval(id)
  }, [finishedAt])
  return <>{fmtMs((finishedAt ?? now) - startedAt)}</>
}

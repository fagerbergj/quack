import type { MemoryWeekStats } from '../api'

export interface MemoryStatsHeaderProps {
  weeks: MemoryWeekStats[]
  loading: boolean
  error: string | null
}

// pct formats a 0..1 fraction as a whole-percent string, or a dash when nothing was
// judged this week (precision is 0 then, and 0% would misread as "confirmed bad").
function pct(week: MemoryWeekStats | undefined): string {
  if (!week) return '—'
  if (week.supported + week.contradicted + week.not_relevant === 0) return '—'
  return `${Math.round(week.precision * 100)}%`
}

// This week's precision up front, vote counts behind a <details> disclosure (no extra
// click chrome, no tooltip-on-touch problem at 390px), last 4 weeks as plain numbers.
export function MemoryStatsHeader({ weeks, loading, error }: MemoryStatsHeaderProps) {
  if (loading) {
    return (
      <div className="px-3 py-2 border-b border-gray-200 dark:border-gray-700 text-xs text-gray-500 dark:text-gray-400">
        Loading recall stats…
      </div>
    )
  }
  if (error) {
    return (
      <div className="px-3 py-2 border-b border-gray-200 dark:border-gray-700 text-xs text-gray-500 dark:text-gray-400">
        Recall stats unavailable
      </div>
    )
  }

  const thisWeek = weeks[weeks.length - 1]
  const last4 = weeks.slice(-4)

  return (
    <details className="px-3 py-2 border-b border-gray-200 dark:border-gray-700 text-xs text-gray-600 dark:text-gray-300">
      <summary className="flex flex-wrap items-center gap-x-4 gap-y-1 cursor-pointer list-none [&::-webkit-details-marker]:hidden">
        <span title="Of every recall the judge ruled on this week (not-relevant included), the share that actually helped">
          Precision <strong className="text-gray-900 dark:text-white">{pct(thisWeek)}</strong>
        </span>
        <span className="flex flex-wrap items-center gap-x-2 text-gray-500 dark:text-gray-400">
          {last4.map(w => (
            <span key={w.week} title={w.week}>{pct(w)}</span>
          ))}
        </span>
      </summary>
      <div className="mt-1.5 text-gray-500 dark:text-gray-400">
        {thisWeek ? (
          <span>
            {thisWeek.recalls} recalled · {thisWeek.supported} supported · {thisWeek.contradicted} contradicted ·{' '}
            {thisWeek.not_relevant} not relevant · {thisWeek.minted} minted · {thisWeek.invalidated} invalidated
          </span>
        ) : (
          <span>No recall data yet</span>
        )}
      </div>
    </details>
  )
}

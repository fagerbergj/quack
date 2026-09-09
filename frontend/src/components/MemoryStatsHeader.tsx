import type { MemoryWeekStats } from '../api'

export interface MemoryStatsHeaderProps {
  weeks: MemoryWeekStats[]
  loading: boolean
  error: string | null
}

// pct formats a 0..1 fraction as a whole-percent string, or a dash when no
// ruled-on votes or recalls this week (precision/support_share are 0 then, and 0%
// would misread as "confirmed bad" rather than "no data yet").
function pct(week: MemoryWeekStats | undefined): string {
  if (!week) return '—'
  if (week.supported + week.contradicted === 0) return '—'
  return `${Math.round(week.precision * 100)}%`
}

function supportSharePct(week: MemoryWeekStats | undefined): string {
  if (!week) return '—'
  if (week.recalls === 0) return '—'
  return `${Math.round(week.support_share * 100)}%`
}

// MemoryStatsHeader (#1267): this week's recall precision and support share
// up front, the vote counts behind them in a <details> disclosure (no extra
// click chrome, no tooltip-on-touch problem at 390px), and the last 4 weeks
// as plain numbers - a sparkline would need a charting dependency for four
// data points.
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
        <span title="Of the recalls the judge ruled on this week, the share it found right (not-relevant votes excluded)">
          Precision <strong className="text-gray-900 dark:text-white">{pct(thisWeek)}</strong>
        </span>
        <span title="Of everything recalled this week, the share that actually helped (unvoted and not-relevant recalls count as no help)">
          Support share <strong className="text-gray-900 dark:text-white">{supportSharePct(thisWeek)}</strong>
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

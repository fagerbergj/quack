import type { Usage } from '../generated'
import { cacheRate } from '../state/chatStore'

export interface UsageSummaryProps {
  // The current/most-recent turn's model chip(s) - see chatStore.sessionModels.
  models: string[]
  // Chat-wide token aggregate (ChatDetail.usage) - a load-time snapshot, not
  // updated live while a run streams.
  usage?: Usage
}

// UsageSummary is the chat header's model chip(s) + session token total,
// expandable (native <details> - no JS state needed) to the
// input/output/reasoning/cached split and cache-hit rate. Renders nothing
// for a chat with no run yet (no models, no tokens).
export function UsageSummary({ models, usage }: UsageSummaryProps) {
  const total = usage?.total_tokens ?? 0
  if (models.length === 0 && total <= 0) return null
  const rate = cacheRate(usage)

  return (
    <div className="flex items-center gap-1.5 text-[10px] text-gray-400 dark:text-gray-500 flex-shrink-0">
      {models.map(m => (
        <span
          key={m}
          className="font-mono px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-700 truncate max-w-[140px]"
          title={m}
        >
          {m}
        </span>
      ))}
      {total > 0 && (
        <details className="relative">
          <summary
            className="list-none cursor-pointer tabular-nums hover:text-gray-600 dark:hover:text-gray-300 select-none"
            title="Session token usage - click to expand"
          >
            {total.toLocaleString()} tok
          </summary>
          <div className="absolute right-0 z-20 mt-1 w-52 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg p-2.5 space-y-1 text-gray-600 dark:text-gray-300">
            <UsageRow label="Input" value={usage?.input_tokens} />
            <UsageRow label="Output" value={usage?.output_tokens} />
            <UsageRow label="Reasoning" value={usage?.reasoning_tokens} />
            <UsageRow label="Cached" value={usage?.cached_tokens} />
            {rate != null && (
              <div className="pt-1 mt-1 border-t border-gray-100 dark:border-gray-700 flex items-center justify-between">
                <span>Cache rate</span>
                <span className="tabular-nums">{rate}%</span>
              </div>
            )}
          </div>
        </details>
      )}
    </div>
  )
}

function UsageRow({ label, value }: { label: string; value?: number }) {
  if (value == null) return null
  return (
    <div className="flex items-center justify-between">
      <span>{label}</span>
      <span className="tabular-nums">{value.toLocaleString()}</span>
    </div>
  )
}

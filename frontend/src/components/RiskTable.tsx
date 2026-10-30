import type { RiskRow } from './A2uiSurface'
import { Badge, type BadgeTone } from './ui/badge'

const RISK_TONE: Record<RiskRow['risk'], BadgeTone> = { low: 'green', medium: 'amber', high: 'red' }

// One card per change rather than a table: five columns of prose do not fit a chat column.
export function RiskTable({ rows, basis }: { rows: RiskRow[]; basis?: 'diff-only' | 'repo' }) {
  return (
    <div className="space-y-2">
      {basis === 'diff-only' && <p className="text-xs text-gray-500 dark:text-gray-400">Blast radius is diff-only: it covers code visible in the PR, not callers elsewhere in the repo.</p>}
      <ul className="space-y-2">
        {rows.map((r, i) => (
          <li key={i} className="rounded-lg border border-gray-200 p-3 dark:border-gray-700">
            <div className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 break-all font-mono text-xs text-gray-900 dark:text-gray-100">{r.change}</span>
              <Badge>{r.type}</Badge>
              <Badge tone={RISK_TONE[r.risk]}>{r.risk} risk</Badge>
            </div>
            <p className="mt-2 text-sm text-gray-800 dark:text-gray-200">{r.reason}</p>
            <dl className="mt-2 space-y-0.5 text-xs text-gray-600 dark:text-gray-300">
              <div><dt className="inline font-medium">Blast radius: </dt><dd className="inline">{r.blast}</dd></div>
              {r.tests && <div><dt className="inline font-medium">Tests: </dt><dd className="inline">{r.tests}</dd></div>}
            </dl>
          </li>
        ))}
      </ul>
    </div>
  )
}

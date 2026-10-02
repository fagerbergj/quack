import type { ChangeType } from './diagramSource'

export interface RiskRow { change: string; type: ChangeType; risk: 'low' | 'medium' | 'high'; reason: string; blast: string; tests?: string }

const RISK_STYLE: Record<RiskRow['risk'], string> = {
  low: 'bg-green-100 text-green-900 border-green-600 dark:bg-green-900/40 dark:text-green-200',
  medium: 'bg-amber-100 text-amber-900 border-amber-600 dark:bg-amber-900/40 dark:text-amber-200',
  high: 'bg-red-100 text-red-900 border-red-600 dark:bg-red-900/40 dark:text-red-200',
}

// One card per change rather than a table: five columns of prose do not fit a chat column.
export function RiskTable({ rows, basis }: { rows: RiskRow[]; basis?: 'diff-only' | 'repo' }) {
  return (
    <div className="space-y-2">
      {basis === 'diff-only' && <p className="text-xs text-gray-500 dark:text-gray-400">Blast radius is diff-only: it covers code visible in the PR, not callers elsewhere in the repo.</p>}
      <ul className="space-y-2">
        {rows.map(r => (
          <li key={r.change} className="rounded-lg border border-gray-200 p-3 dark:border-gray-700">
            <div className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 break-all font-mono text-xs text-gray-900 dark:text-gray-100">{r.change}</span>
              <span className="rounded bg-gray-100 px-1.5 py-0.5 text-[11px] dark:bg-gray-700">{r.type}</span>
              <span className={`rounded border px-1.5 py-0.5 text-[11px] font-medium ${RISK_STYLE[r.risk]}`}>{r.risk} risk</span>
            </div>
            <p className="mt-1.5 text-sm text-gray-800 dark:text-gray-200">{r.reason}</p>
            <dl className="mt-1.5 space-y-0.5 text-xs text-gray-600 dark:text-gray-300">
              <div><dt className="inline font-medium">Blast radius: </dt><dd className="inline">{r.blast}</dd></div>
              {r.tests && <div><dt className="inline font-medium">Tests: </dt><dd className="inline">{r.tests}</dd></div>}
            </dl>
          </li>
        ))}
      </ul>
    </div>
  )
}

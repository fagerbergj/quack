import { useEffect, useState } from 'react'
import { api, listChatArtifactsShared } from '../api'
import { tryParseJSON } from '../lib/json'
import { nodeJudgeRounds, parseJudgeRound, pct, type JudgeRoundDetail, type JudgeRoundRef } from '../lib/judgeRounds'
import { Icon } from './Icon'

// One round: verdict, the criterion that sank it, and every criterion's score with the judge's reason a tap away.
export function JudgeRoundBreakdown({ detail }: { detail: JudgeRoundDetail }) {
  const failedOn = detail.criteria.filter(c => c.failed).map(c => c.name)
  return (
    <section aria-label={`Judge round ${detail.round}`} className="space-y-1">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs">
        <span className="font-semibold text-gray-700 dark:text-gray-200">Round {detail.round}</span>
        <span className={`inline-flex items-center gap-0.5 font-medium ${detail.passed ? 'text-green-700 dark:text-green-400' : 'text-red-700 dark:text-red-400'}`}>
          <Icon name={detail.passed ? 'check' : 'close'} className="w-3 h-3" />
          {detail.passed ? 'passed' : 'failed'}{detail.score != null && ` ${pct(detail.score)}`}
        </span>
        {failedOn.length > 0 && (
          <span className="min-w-0 text-gray-600 dark:text-gray-300 [overflow-wrap:anywhere]">
            on <span className="font-mono">{failedOn.join(', ')}</span>
          </span>
        )}
      </div>
      <ul className="space-y-0.5 max-w-xl">
        {detail.criteria.map(c => (
          <li key={c.name}>
            <details>
              <summary className="list-none cursor-pointer min-h-[32px] flex items-center gap-2 text-2xs rounded hover:bg-gray-50 dark:hover:bg-gray-700/40">
                <span className={`min-w-0 flex-1 truncate font-mono ${c.failed ? 'text-red-700 dark:text-red-400 font-semibold' : 'text-gray-600 dark:text-gray-300'}`} title={c.name}>
                  {c.failed && <Icon name="close" className="w-3 h-3 inline -mt-0.5 mr-0.5" />}{c.name}
                </span>
                <span aria-hidden className="w-16 sm:w-24 h-1.5 shrink-0 rounded-full bg-gray-100 dark:bg-gray-700 overflow-hidden">
                  <span
                    className={`block h-full rounded-full ${c.failed ? 'bg-red-600 dark:bg-red-400' : 'bg-gray-400 dark:bg-gray-500'}`}
                    style={{ width: `${Math.round((c.score ?? 0) * 100)}%` }}
                  />
                </span>
                <span className="w-9 shrink-0 text-right tabular-nums text-gray-600 dark:text-gray-300">{c.score != null ? pct(c.score) : '-'}</span>
              </summary>
              {c.feedback && (
                <p className="pb-1 text-2xs text-gray-500 dark:text-gray-400 whitespace-pre-wrap [overflow-wrap:anywhere]">{c.feedback}</p>
              )}
            </details>
          </li>
        ))}
      </ul>
    </section>
  )
}

function useNodeJudgeRounds(chatId: string | undefined, nodeId: string, startedAt?: number, finishedAt?: number) {
  const [refs, setRefs] = useState<JudgeRoundRef[]>([])
  useEffect(() => {
    if (!chatId || startedAt == null || finishedAt == null) return
    let cancelled = false
    listChatArtifactsShared(chatId)
      .then(l => { if (!cancelled) setRefs(nodeJudgeRounds(l.data ?? [], nodeId, startedAt, finishedAt)) })
      .catch(() => {})
    return () => { cancelled = true }
  }, [chatId, nodeId, startedAt, finishedAt])
  return refs
}

// A finished node's judge rounds from the judge_round artifacts, so a reloaded turn (which has no live judge
// cards) still shows them. Bodies load on first open: a long chat renders dozens of node cards. No chatId
// (a Storybook fixture) or no finishedAt (still running) renders nothing.
export function NodeQualityCheck({ chatId, nodeId, startedAt, finishedAt }: {
  chatId?: string
  nodeId: string
  startedAt?: number
  finishedAt?: number
}) {
  const refs = useNodeJudgeRounds(chatId, nodeId, startedAt, finishedAt)
  const [details, setDetails] = useState<JudgeRoundDetail[] | null>(null)
  const [failed, setFailed] = useState(false)
  useEffect(() => { setDetails(null); setFailed(false) }, [refs])

  if (!chatId || finishedAt == null || refs.length === 0) return null
  const load = () => {
    if (details) return
    Promise.all(refs.map(r => api.getArtifactText(chatId, r.name).then(t => parseJudgeRound(tryParseJSON(t), r.round))))
      .then(ds => setDetails(ds.filter(d => d != null)))
      .catch(() => setFailed(true))
  }
  return (
    <details
      className="border-t border-gray-100 dark:border-gray-700"
      onToggle={e => { if (e.currentTarget.open) load() }}
    >
      <summary className="cursor-pointer select-none min-h-[44px] px-4 flex items-center gap-2 text-2xs text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700/40">
        <span className="font-semibold">Quality check</span>
        <span className="tabular-nums text-gray-500 dark:text-gray-400">{refs.length} round{refs.length === 1 ? '' : 's'}</span>
        <span className="ml-auto text-gray-500 dark:text-gray-400">per-criterion scores</span>
      </summary>
      <div className="px-4 pb-3 space-y-3">
        {failed ? (
          <p className="text-2xs text-red-700 dark:text-red-400">Couldn't load the judge rounds.</p>
        ) : details == null ? (
          <p role="status" className="text-2xs text-gray-500 dark:text-gray-400">Loading judge rounds…</p>
        ) : (
          details.map(d => <JudgeRoundBreakdown key={d.round} detail={d} />)
        )}
      </div>
    </details>
  )
}

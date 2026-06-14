import { useState, useEffect } from 'react'
import { AssistantText, ActivityList } from './AgentParts'
import type { NodeState, NodeStatus } from '../state/chatStore'
import type { AgentRun } from './messageParts'
import type { DagNodeDef } from '../state/agentStream'

function fmtMs(ms: number): string {
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  const rem = Math.floor(s % 60)
  if (m < 60) return `${m}m ${rem}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m ${rem}s`
}

// LiveTimer ticks every 100ms while running, then freezes on the final value.
function LiveTimer({ startedAt, finishedAt }: { startedAt: number; finishedAt?: number }) {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    if (finishedAt != null) return
    const id = setInterval(() => setNow(Date.now()), 100)
    return () => clearInterval(id)
  }, [finishedAt])
  return <>{fmtMs((finishedAt ?? now) - startedAt)}</>
}

function StatusBadge({ status }: { status: NodeStatus }) {
  const styles: Record<NodeStatus, string> = {
    queued:  'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400',
    running: 'bg-blue-100 text-blue-600 dark:bg-blue-900/40 dark:text-blue-400',
    done:    'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400',
    failed:  'bg-red-100 text-red-600 dark:bg-red-900/40 dark:text-red-400',
  }
  const labels: Record<NodeStatus, string> = {
    queued: 'queued', running: 'running…', done: 'done', failed: 'failed',
  }
  return (
    <span className={`text-[10px] font-medium px-1.5 py-0.5 rounded ${styles[status]}`}>
      {labels[status]}
    </span>
  )
}

function Spinner() {
  return (
    <span className="flex items-center gap-0.5">
      <span className="w-1.5 h-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:-0.3s]" />
      <span className="w-1.5 h-1.5 rounded-full bg-blue-400 animate-bounce [animation-delay:-0.15s]" />
      <span className="w-1.5 h-1.5 rounded-full bg-blue-400 animate-bounce" />
    </span>
  )
}

// RunTimer shows a per-run elapsed timer: live while the run is open, frozen on
// its final duration once complete. Floated right within a card summary.
function RunTimer({ run }: { run: AgentRun }) {
  if (run.done) {
    return run.durationMs != null
      ? <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums ml-auto">{fmtMs(run.durationMs)}</span>
      : null
  }
  return run.startedAt != null
    ? <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums ml-auto"><LiveTimer startedAt={run.startedAt} /></span>
    : null
}

function agentLabel(name: string): string {
  if (name === 'web-researcher') return 'Web researcher'
  if (name === 'synthesizer') return 'Synthesizer'
  return name
}

// ── per-run stage cards ──────────────────────────────────────────────────────

// ResearchCard renders the worker stage (draft + finalize): its activity, plus
// the node's answer expanded when this is the final node.
function ResearchCard({ run, running, answer, isFinal }: { run: AgentRun; running: boolean; answer: string; isFinal: boolean }) {
  const empty = run.activity.length === 0
  return (
    <div>
      {empty ? (
        running ? <div className="px-4 py-3 text-xs text-gray-400 dark:text-gray-500">starting…</div> : null
      ) : (
        <details open={running} className="not-prose">
          <summary className="cursor-pointer select-none px-4 py-2 text-xs text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300">
            {running ? 'activity…' : `${run.activity.length} step${run.activity.length === 1 ? '' : 's'}`}
          </summary>
          <div className="px-4 pb-3">
            <ActivityList activity={run.activity} />
          </div>
        </details>
      )}
      {isFinal && answer && (
        <div className="px-4 py-3 border-t border-gray-100 dark:border-gray-700">
          <AssistantText text={answer} />
        </div>
      )}
    </div>
  )
}

function SelfCritiqueCard({ run, running }: { run: AgentRun; running: boolean }) {
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-amber-600 dark:text-amber-400 uppercase tracking-wide">
            Self-critique
          </span>
          {running ? <Spinner /> : (
            <span className="text-[10px] text-gray-400 dark:text-gray-500">{run.changed ? 'revised' : 'no changes'}</span>
          )}
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3"><ActivityList activity={run.activity} /></div>
        )}
      </details>
    </div>
  )
}

function JudgeCard({ run, running }: { run: AgentRun; running: boolean }) {
  if (run.done && run.status === 'unavailable') {
    return (
      <div className="border-t border-gray-100 dark:border-gray-700 px-4 py-2 bg-yellow-50 dark:bg-yellow-900/15">
        <span className="text-[10px] font-semibold text-yellow-700 dark:text-yellow-400 uppercase tracking-wide">
          ⚠ Judge unavailable · round {run.round}
        </span>
        <div className="text-[11px] text-yellow-700 dark:text-yellow-400/90 mt-0.5">
          Answer surfaced without quality vetting — {run.reason}
        </div>
      </div>
    )
  }
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-purple-600 dark:text-purple-400 uppercase tracking-wide">
            Judge · round {run.round}
          </span>
          {running ? <Spinner /> : run.score != null && (
            <span className={`text-[10px] font-medium ${run.passed ? 'text-green-600 dark:text-green-400' : 'text-red-500 dark:text-red-400'}`}>
              {run.passed ? '✓' : '✗'} {(run.score * 100).toFixed(0)}%
            </span>
          )}
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3"><ActivityList activity={run.activity} /></div>
        )}
      </details>
      {/* Fail reason always visible (not hidden behind the collapsed card). */}
      {run.done && run.feedback && run.feedback !== 'None' && (
        <div className="px-4 pt-0 pb-2 text-[11px] text-gray-500 dark:text-gray-400 italic">{run.feedback}</div>
      )}
    </div>
  )
}

function RevisionCard({ run, running }: { run: AgentRun; running: boolean }) {
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-blue-500 dark:text-blue-400 uppercase tracking-wide">
            ↺ Revised · round {run.round}
          </span>
          {running && <Spinner />}
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3"><ActivityList activity={run.activity} /></div>
        )}
      </details>
    </div>
  )
}

// ── DagNode ─────────────────────────────────────────────────────────────────

interface Props {
  node: DagNodeDef
  state: NodeState
  runs: AgentRun[]
  answer: string
  isFinal: boolean
}

export function DagNode({ node, state, runs, answer, isFinal }: Props) {
  const running = state.status === 'running'
  // The actively-streaming run is the last not-yet-done run while the node runs.
  const activeIdx = running ? runs.map(r => r.done).lastIndexOf(false) : -1

  return (
    <div className={`rounded-xl border shadow-sm overflow-hidden ${
      isFinal
        ? 'border-indigo-200 dark:border-indigo-800 bg-white dark:bg-gray-800'
        : 'border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800'
    }`}>
      {/* Node header */}
      <div className="flex items-center gap-2 px-4 py-3 border-b border-gray-100 dark:border-gray-700">
        <span className="text-xs font-semibold text-gray-700 dark:text-gray-200">
          {agentLabel(node.agent)}
        </span>
        <StatusBadge status={state.status} />
        {running && runs.length === 0 && <Spinner />}
        <div className="ml-auto flex items-center gap-2">
          {state.model && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 font-mono truncate max-w-[120px]" title={state.model}>
              {state.model}
            </span>
          )}
          {state.finishReason === 'MAX_TOKENS' && (
            <span className="text-[10px] font-medium text-amber-600 dark:text-amber-400" title="Response was truncated at the token limit">
              truncated
            </span>
          )}
          {state.judgeRounds != null && state.judgeRounds > 0 && state.judgePassed === false && (
            <span
              className="text-[10px] font-medium text-amber-600 dark:text-amber-400"
              title={`Judge rejected this output after ${state.judgeRounds} round${state.judgeRounds === 1 ? '' : 's'}${state.judgeFinalScore != null ? ` (final score ${(state.judgeFinalScore * 100).toFixed(0)}%)` : ''} — surfaced unvetted`}
            >
              ⚠ unvetted
            </span>
          )}
          {(state.totalTokens != null && state.totalTokens > 0)
            ? <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">{state.totalTokens.toLocaleString()} tok</span>
            : state.outputChars != null && state.outputChars > 0
              ? <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">~{Math.round(state.outputChars / 4).toLocaleString()} tok</span>
              : null
          }
          {state.startedAt != null && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
              <LiveTimer startedAt={state.startedAt} finishedAt={state.finishedAt} />
            </span>
          )}
        </div>
      </div>

      {/* Task description */}
      <div className="px-4 py-2 text-xs text-gray-500 dark:text-gray-400 border-b border-gray-100 dark:border-gray-700">
        {node.task}
      </div>

      {/* Per-run stage cards */}
      {runs.map((run, i) => {
        const runRunning = i === activeIdx
        switch (run.stage) {
          case 'self_refine': return <SelfCritiqueCard key={run.runId} run={run} running={runRunning} />
          case 'judge':       return <JudgeCard key={run.runId} run={run} running={runRunning} />
          case 'revise':      return <RevisionCard key={run.runId} run={run} running={runRunning} />
          default:            return <ResearchCard key={run.runId} run={run} running={runRunning} answer={answer} isFinal={isFinal} />
        }
      })}

      {/* Failed state */}
      {state.status === 'failed' && state.error && (
        <div className="px-4 py-2 text-xs text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/20">
          {state.error}
        </div>
      )}
    </div>
  )
}

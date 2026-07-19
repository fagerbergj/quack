import { memo, useState } from 'react'
import { AssistantText, ActivityList } from './AgentParts'
import { Expandable } from './Expandable'
import { NodePopup } from './NodePopup'
import type { NodeState, NodeStatus } from '../state/chatStore'
import { agentLabel, type AgentRun } from './messageParts'
import { type DagNodeDef } from '../state/agentStream'
import { fmtMs, LiveTimer } from '../utils/timer'

function StatusBadge({ status }: { status: NodeStatus }) {
  const styles: Record<NodeStatus, string> = {
    queued:  'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400',
    running: 'bg-blue-100 text-blue-600 dark:bg-blue-900/40 dark:text-blue-400',
    needs_input: 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400',
    paused:  'bg-sky-100 text-sky-700 dark:bg-sky-900/40 dark:text-sky-400',
    done:    'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400',
    failed:  'bg-red-100 text-red-600 dark:bg-red-900/40 dark:text-red-400',
    cancelled: 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300',
  }
  const labels: Record<NodeStatus, string> = {
    queued: 'queued', running: 'running…', done: 'done', failed: 'failed',
    needs_input: 'waiting for you', paused: 'paused', cancelled: 'stopped',
  }
  return (
    <span className={`text-[10px] font-medium px-1.5 py-0.5 rounded ${styles[status]}`}>
      {labels[status]}
    </span>
  )
}

// QueuedBadge indicates one or more not-yet-delivered queued messages —
// visible at a glance on the card, edited/removed in the popup.
function QueuedBadge({ count }: { count: number }) {
  if (count === 0) return null
  return (
    <span
      className="text-[10px] font-medium px-1.5 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400"
      title={`${count} queued message${count === 1 ? '' : 's'} — delivered at the node's next turn boundary`}
    >
      ✉ {count}
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

// RunModel shows the model that produced a run, once known (set on agent_complete).
function RunModel({ run }: { run: AgentRun }) {
  if (!run.model) return null
  return (
    <span className="text-[10px] text-gray-400 dark:text-gray-500 font-mono truncate max-w-[100px]" title={run.model}>
      {run.model}
    </span>
  )
}

// ── per-run stage cards ──────────────────────────────────────────────────────

// WorkerCard renders the worker stage (draft + finalize): its activity —
// including any ask_advisor consults, which show up as ordinary tool calls. The
// node's vetted answer is rendered separately at the foot of the node (NodeAnswer),
// so it sits below the judge rather than inside the worker card. Like every other
// stage card it carries its own labeled header — without one, its activity rows
// visually attach to whatever labeled card rendered above it.
// Memoized so an event on one run (a new run object, per messageParts' mapRun)
// only re-renders that run's own card — sibling runs keep the same `run`
// reference and bail out of the shallow prop compare, instead of every card in
// the node re-rendering on every tool-call/thinking event (#379).
const WorkerCard = memo(function WorkerCard({ run, running }: { run: AgentRun; running: boolean }) {
  const empty = run.activity.length === 0
  if (empty) {
    return running ? <div className="px-4 py-3 text-xs text-gray-400 dark:text-gray-500">starting…</div> : null
  }
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-sky-600 dark:text-sky-400 uppercase tracking-wide">
            Work
          </span>
          {running && <Spinner />}
          <span className="text-xs text-gray-400 dark:text-gray-500">
            {running ? 'activity…' : `${run.activity.length} step${run.activity.length === 1 ? '' : 's'}`}
          </span>
          <RunModel run={run} />
          <RunTimer run={run} />
        </summary>
        <div className="px-4 pb-3">
          <ActivityList activity={run.activity} />
        </div>
      </details>
    </div>
  )
})

// NodeAnswer renders a node's vetted output as a collapsible at the foot of the
// node. Shown for every node so each specialist's answer is inspectable — not just
// the final one (whose answer also appears in the main message bubble).
function NodeAnswer({ answer }: { answer: string }) {
  if (!answer) return null
  return (
    <details className="not-prose border-t border-gray-100 dark:border-gray-700">
      <summary className="cursor-pointer select-none px-4 py-2 text-xs text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300">
        answer
      </summary>
      <div className="px-4 pb-3">
        <Expandable maxHeight={360}>
          <AssistantText text={answer} />
        </Expandable>
      </div>
    </details>
  )
}

const JudgeCard = memo(function JudgeCard({ run, running }: { run: AgentRun; running: boolean }) {
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
          <RunModel run={run} />
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3"><ActivityList activity={run.activity} /></div>
        )}
      </details>
      {/* Fail reason always visible (not hidden behind the collapsed card). */}
      {run.done && run.feedback && run.feedback !== 'None' && (
        <div className="px-4 pt-0 pb-2 text-[11px] text-gray-500 dark:text-gray-400 italic">
          <Expandable maxHeight={120}>{run.feedback}</Expandable>
        </div>
      )}
    </div>
  )
})

const RevisionCard = memo(function RevisionCard({ run, running }: { run: AgentRun; running: boolean }) {
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-blue-500 dark:text-blue-400 uppercase tracking-wide">
            ↺ Revised · round {run.round}
          </span>
          {running && <Spinner />}
          <RunModel run={run} />
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3"><ActivityList activity={run.activity} /></div>
        )}
      </details>
    </div>
  )
})


// RetryControl re-runs a FINISHED node (failed or done) and everything downstream
// of it. Plain retry reuses the node's task; "retry with guidance" reveals an inline
// input (guidance == steer, on a finished node). Shown only on a live turn.
function RetryControl({ nodeId, onRetry }: {
  nodeId: string
  onRetry: (nodeId: string, guidance?: string) => void
}) {
  const [guiding, setGuiding] = useState(false)
  const [text, setText] = useState('')
  const send = () => {
    onRetry(nodeId, text.trim() || undefined)
    setText('')
    setGuiding(false)
  }
  if (guiding) {
    return (
      <div className="flex items-center gap-2 px-4 py-2 border-b border-gray-100 dark:border-gray-700 bg-indigo-50/50 dark:bg-indigo-900/10">
        <input
          autoFocus
          value={text}
          onChange={e => setText(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') { e.preventDefault(); send() }
            if (e.key === 'Escape') { setGuiding(false); setText('') }
          }}
          placeholder="Retry with guidance (optional) — re-runs this node + downstream…"
          className="flex-1 min-w-0 text-xs px-2 py-1 rounded border border-indigo-300 dark:border-indigo-700 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-indigo-400"
        />
        <button onClick={send} className="text-[11px] font-medium text-indigo-700 dark:text-indigo-400 hover:underline">retry</button>
        <button onClick={() => { setGuiding(false); setText('') }} className="text-[11px] text-gray-400 dark:text-gray-500 hover:underline">cancel</button>
      </div>
    )
  }
  return (
    <div className="flex items-center gap-3 px-4 py-1.5 border-b border-gray-100 dark:border-gray-700">
      <button onClick={() => onRetry(nodeId)} title="Re-run this node and everything downstream of it (reuses the rest)"
        className="text-[10px] font-medium text-indigo-600 dark:text-indigo-400 hover:underline">
        ↻ retry
      </button>
      <button onClick={() => setGuiding(true)} title="Re-run this node with new guidance"
        className="text-[10px] font-medium text-indigo-600 dark:text-indigo-400 hover:underline">
        ↻ retry with guidance…
      </button>
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
  onCancel?: (nodeId: string) => void
  onPause?: (nodeId: string) => void
  onResume?: (nodeId: string) => void
  onQueueMessage?: (nodeId: string, text: string) => void
  onEditQueuedMessage?: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage?: (nodeId: string, messageId: string) => void
  onEditTask?: (nodeId: string, task: string) => void
  onRetry?: (nodeId: string, guidance?: string) => void
}

export function DagNode({
  node, state, runs, answer, isFinal,
  onCancel, onPause, onResume, onQueueMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditTask,
  onRetry,
}: Props) {
  const running = state.status === 'running'
  // Retry (→ queued) is legal from done, failed, or cancelled — see dag.CanTransition.
  const finished = state.status === 'done' || state.status === 'failed' || state.status === 'cancelled'
  // The actively-streaming run is the last not-yet-done run while the node runs.
  const activeIdx = running ? runs.map(r => r.done).lastIndexOf(false) : -1
  const [popupOpen, setPopupOpen] = useState(false)
  const pendingQueueCount = (state.queue ?? []).filter(m => !m.delivered).length

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
        <QueuedBadge count={pendingQueueCount} />
        {state.steers && state.steers.length > 0 && (
          <span
            className="text-[10px] font-medium text-amber-600 dark:text-amber-400"
            title={`Queued message(s) delivered:\n${state.steers.join('\n')}`}
          >
            ↻ steered{state.steers.length > 1 ? ` ×${state.steers.length}` : ''}
          </span>
        )}
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
          {state.totalTokens != null && state.totalTokens > 0 && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">{state.totalTokens.toLocaleString()} tok</span>
          )}
          {/* A finished node shows the server-measured duration (reconnect-proof);
              a running one ticks live from the server start time. */}
          {(state.finishedAt != null && state.serverDurationMs != null) ? (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">{fmtMs(state.serverDurationMs)}</span>
          ) : state.startedAt != null ? (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
              <LiveTimer startedAt={state.startedAt} finishedAt={state.finishedAt} />
            </span>
          ) : null}
        </div>
      </div>

      {/* Node summary — click to open the popup (#384): the full prompt as
          markdown, plus (on a live turn) cancel/pause/resume and the message
          queue. Optimized for clean presentation, not information density —
          the full prompt is always recoverable from the trace. */}
      <button
        onClick={() => setPopupOpen(true)}
        className="w-full text-left px-4 py-2 text-xs text-gray-500 dark:text-gray-400 border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700/40 truncate"
        title="View prompt and controls"
      >
        {node.task}
      </button>
      {popupOpen && (
        <NodePopup
          node={node}
          state={state}
          onClose={() => setPopupOpen(false)}
          onCancel={onCancel}
          onPause={onPause}
          onResume={onResume}
          onQueueMessage={onQueueMessage}
          onEditQueuedMessage={onEditQueuedMessage}
          onRemoveQueuedMessage={onRemoveQueuedMessage}
          onEditTask={onEditTask}
        />
      )}

      {/* Retry a finished node (failed or done) + its downstream, on a live turn */}
      {finished && onRetry && (
        <RetryControl nodeId={node.id} onRetry={onRetry} />
      )}

      {/* Per-run stage cards */}
      {runs.map((run, i) => {
        const runRunning = i === activeIdx
        switch (run.stage) {
          case 'judge':   return <JudgeCard key={run.runId} run={run} running={runRunning} />
          case 'revise':  return <RevisionCard key={run.runId} run={run} running={runRunning} />
          default:        return <WorkerCard key={run.runId} run={run} running={runRunning} />
        }
      })}

      {/* Vetted answer (below the stage cards, for every node) */}
      <NodeAnswer answer={answer} />

      {/* Failed state */}
      {state.status === 'failed' && state.error && (
        <div className="px-4 py-2 text-xs text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/20">
          {state.error}
        </div>
      )}

      {/* Stopped by the user (node_cancelled) — rendered neutrally, not as an error */}
      {state.status === 'cancelled' && (
        <div className="px-4 py-2 text-xs text-gray-500 dark:text-gray-400 bg-gray-50 dark:bg-gray-800/40">
          Stopped by you
        </div>
      )}
    </div>
  )
}

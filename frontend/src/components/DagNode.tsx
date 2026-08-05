import { memo, useEffect, useRef, useState } from 'react'
import { AssistantText, ActivityList, LiveStatusLine, AcpBadge, isAcpAgent } from './AgentParts'
import { CopyButton } from './CopyButton'
import { NodePopup } from './NodePopup'
import { StatusDot } from './StatusDot'
import type { NodeState, NodeStatus } from '../state/chatStore'
import { agentLabel, type Activity, type AgentRun } from './messageParts'
import { previewLine } from './toolFormat'
import { type DagNodeDef } from '../state/agentStream'
import { fmtMs, LiveTimer } from '../utils/timer'

// NodeMenu is the node's ⋮ overflow menu: one click for pause/resume/cancel
// (no popup round-trip), with "queue a message…" / "edit prompt" / "answer
// question…" opening the popup only when they need its input/editor. Hidden
// entirely on a terminal node (done/failed/cancelled) - nothing left to do.
function NodeMenu({
  nodeId, status, onCancel, onPause, onResume, canQueue, canEdit, canAnswer, onOpenPopup,
}: {
  nodeId: string
  status: NodeStatus
  onCancel?: (nodeId: string) => void
  onPause?: (nodeId: string) => void
  onResume?: (nodeId: string) => void
  canQueue: boolean
  canEdit: boolean
  canAnswer: boolean
  onOpenPopup: () => void
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const terminal = status === 'done' || status === 'failed' || status === 'cancelled'
  if (terminal) return null

  const running = status === 'running'
  const paused = status === 'paused'
  const cancellable = running || paused || status === 'queued' || status === 'needs_input'
  const hasSecondary = canAnswer || canQueue || canEdit

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen(o => !o)}
        aria-label="Node actions"
        aria-haspopup="menu"
        aria-expanded={open}
        className={`w-5 h-5 flex items-center justify-center rounded text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-opacity ${open ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 focus:opacity-100'}`}
      >
        ⋮
      </button>
      {open && (
        <div role="menu" className="absolute z-20 right-0 mt-1 w-48 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg py-1 text-xs">
          {running && onPause && (
            <button role="menuitem" onClick={() => { onPause(nodeId); setOpen(false) }} className="w-full text-left px-3 py-1.5 text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700">
              ⏸ Pause
            </button>
          )}
          {paused && onResume && (
            <button role="menuitem" onClick={() => { onResume(nodeId); setOpen(false) }} className="w-full text-left px-3 py-1.5 text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700">
              ▶ Resume
            </button>
          )}
          {cancellable && onCancel && (
            <button role="menuitem" onClick={() => { onCancel(nodeId); setOpen(false) }} className="w-full text-left px-3 py-1.5 text-red-500 dark:text-red-400 hover:bg-gray-50 dark:hover:bg-gray-700">
              ✕ Cancel
            </button>
          )}
          {hasSecondary && <div className="my-1 border-t border-gray-100 dark:border-gray-700" />}
          {canAnswer && (
            <button role="menuitem" onClick={() => { onOpenPopup(); setOpen(false) }} className="w-full text-left px-3 py-1.5 text-amber-700 dark:text-amber-400 hover:bg-gray-50 dark:hover:bg-gray-700">
              ❓ Answer question…
            </button>
          )}
          {canQueue && (
            <button role="menuitem" onClick={() => { onOpenPopup(); setOpen(false) }} className="w-full text-left px-3 py-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700">
              ✉ Queue a message…
            </button>
          )}
          {canEdit && (
            <button role="menuitem" onClick={() => { onOpenPopup(); setOpen(false) }} className="w-full text-left px-3 py-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700">
              ✎ Edit prompt
            </button>
          )}
        </div>
      )}
    </div>
  )
}

// QueuedBadge indicates one or more not-yet-delivered queued messages -
// visible at a glance on the card, edited/removed in the popup.
function QueuedBadge({ count }: { count: number }) {
  if (count === 0) return null
  return (
    <span
      className="text-[10px] font-medium px-1.5 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400"
      title={`${count} queued message${count === 1 ? '' : 's'} - delivered at the node's next turn boundary`}
    >
      ✉ {count}
    </span>
  )
}

// (Per-run "spinner" dots were removed - redundant with the node header's
// pulsing status dot, they read as a stray extra dot in run-card summaries.)

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

// ContentPopup shows one block of prose (a judge verdict, a node's vetted
// answer) full-size, as an extension of the main chat rather than a bespoke
// modal - the same structure NodePopup uses (#384/#406): a light overlay, a
// close ✕ on its own row (never overlapping the content - the maintainer just
// fixed exactly that overlap on NodePopup), Escape-to-close, click-outside-to-
// close, and the content in a chat-style bubble via AssistantText so it reads
// as formatted markdown.
function ContentPopup({ title, text, onClose }: { title: string; text: string; onClose: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])
  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
      onClick={onClose}
    >
      <div
        className="relative w-full max-w-2xl max-h-[85vh] overflow-y-auto rounded-2xl bg-gray-50 dark:bg-gray-900 shadow-xl px-5 pb-6 pt-2 space-y-2"
        onClick={e => e.stopPropagation()}
      >
        <div className="flex justify-end -mb-1">
          <button
            onClick={onClose}
            aria-label="Close"
            className="flex h-7 w-7 items-center justify-center rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-500 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
          >
            ✕
          </button>
        </div>
        <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
          <div className="flex items-center justify-between mb-2">
            <span className="text-[10px] font-semibold text-gray-400 dark:text-gray-500 uppercase tracking-wide">{title}</span>
            <CopyButton text={text} label={`Copy ${title.toLowerCase()}`} />
          </div>
          <AssistantText text={text} />
        </div>
      </div>
    </div>
  )
}

// CollapsedPreview is the one-line "label + truncated preview" affordance -
// the same compact-collapse ethos as ThinkBlock/ToolBlock (#385/#399) - but
// clicking it opens the full content in a ContentPopup instead of expanding
// inline: a judge verdict or a node's vetted answer reads better full-size
// (it's often the thing the reader most wants to inspect) than height-locked
// inside the node card.
function CollapsedPreview({ label, text, popupTitle }: { label: string; text: string; popupTitle: string }) {
  const [open, setOpen] = useState(false)
  if (!text) return null
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="w-full flex items-center gap-1.5 py-0.5 text-[11px] text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 text-left"
      >
        <span className="italic shrink-0">{label}</span>
        <span className="truncate text-gray-300 dark:text-gray-600">{previewLine(text)}</span>
      </button>
      {open && <ContentPopup title={popupTitle} text={text} onClose={() => setOpen(false)} />}
    </>
  )
}

// ── per-run stage cards ──────────────────────────────────────────────────────

// WorkerCard renders the worker stage's activity as ONE continuous feed -
// including any ask_advisor consults, which show up as ordinary tool calls.
// `runs` is one or more consecutive same-stage worker runs (see groupWorkerRuns):
// a mechanical continuation round (e.g. a deterministic-check retry, #399
// follow-up) hands the worker another tool-bearing turn as a NEW run, but that's
// not a meaningful stage boundary the way a judge-triggered revise is - so
// their activity is concatenated into this one card rather than opening a
// second boxed block. The node's vetted answer is rendered separately at the
// foot of the node (NodeAnswer), so it sits below the judge rather than inside
// the worker card. Like every other stage card it carries its own labeled
// header - without one, its activity rows visually attach to whatever labeled
// card rendered above it.
// Memoized so an event on one run (a new run object, per messageParts' mapRun)
// only re-renders that run's own group - sibling groups keep the same `runs`
// reference and bail out of the shallow prop compare, instead of every card in
// the node re-rendering on every tool-call/thinking event (#379).
const WorkerCard = memo(function WorkerCard({ runs, running }: { runs: AgentRun[]; running: boolean }) {
  const activity: Activity[] = runs.length === 1 ? runs[0].activity : runs.flatMap(r => r.activity)
  const empty = activity.length === 0
  if (empty) {
    return running ? <div className="px-4 py-3 text-xs text-gray-400 dark:text-gray-500">starting…</div> : null
  }
  const model = [...runs].reverse().find(r => r.model)?.model
  const startedAt = runs.find(r => r.startedAt != null)?.startedAt
  const durationMs = running ? undefined : runs.reduce((sum, r) => sum + (r.durationMs ?? 0), 0)
  const timerRun: AgentRun = { ...runs[runs.length - 1], startedAt, durationMs, model, done: !running }
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          {/* Step count (grows live); "running" is already the node header's
              pulsing status dot + the timer - no separate spinner dot here. */}
          <span className="text-xs text-gray-400 dark:text-gray-500">
            {`${activity.length} step${activity.length === 1 ? '' : 's'}`}
          </span>
          <RunModel run={timerRun} />
          <RunTimer run={timerRun} />
        </summary>
        <div className="px-4 pb-3">
          {running ? <LiveStatusLine activity={activity} /> : <ActivityList activity={activity} />}
        </div>
      </details>
    </div>
  )
})

// groupWorkerRuns folds consecutive 'worker'-stage runs into one render group
// so a mechanical continuation round (e.g. a deterministic-check retry) merges
// into its predecessor's activity feed instead of opening a new boxed block -
// judge and revise runs stay their own group (they mark a meaningful stage).
type RunGroup = { stage: AgentRun['stage']; runs: AgentRun[]; activeIdx: number }

function groupWorkerRuns(runs: AgentRun[], activeIdx: number): RunGroup[] {
  const groups: RunGroup[] = []
  runs.forEach((run, i) => {
    const prev = groups[groups.length - 1]
    if (run.stage === 'worker' && prev?.stage === 'worker') {
      prev.runs.push(run)
      if (i === activeIdx) prev.activeIdx = prev.runs.length - 1
    } else {
      groups.push({ stage: run.stage, runs: [run], activeIdx: i === activeIdx ? 0 : -1 })
    }
  })
  return groups
}

// NodeAnswer renders a node's vetted output as a one-line preview at the foot
// of the node - the same collapse-to-one-line ethos as ThinkBlock/ToolBlock
// (#385/#399), extended to the answer (0.9.0 feedback): a truncated preview by
// default, the full answer in a popup on click (maintainer call: the answer
// reads better full-size than height-locked inline). Shown for every node so
// each specialist's answer is inspectable - not just the final one (whose
// answer also appears in the main message bubble).
function NodeAnswer({ answer }: { answer: string }) {
  const [open, setOpen] = useState(false)
  if (!answer) return null
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="w-full flex items-center gap-1.5 px-4 py-2 text-xs text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 border-t border-gray-100 dark:border-gray-700 text-left"
      >
        <span className="shrink-0">answer</span>
        <span className="truncate text-gray-300 dark:text-gray-600">{previewLine(answer, 100)}</span>
      </button>
      {open && <ContentPopup title="Answer" text={answer} onClose={() => setOpen(false)} />}
    </>
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
          Answer surfaced without quality vetting - {run.reason}
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
          {run.score != null && (
            <span className={`text-[10px] font-medium ${run.passed ? 'text-green-600 dark:text-green-400' : 'text-red-500 dark:text-red-400'}`}>
              {run.passed ? '✓' : '✗'} {(run.score * 100).toFixed(0)}%
            </span>
          )}
          <RunModel run={run} />
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3">
            {running ? <LiveStatusLine activity={run.activity} /> : <ActivityList activity={run.activity} />}
          </div>
        )}
      </details>
      {/* Verdict collapses to one line, like ThinkBlock - full reasoning opens
          in a popup, rendered as markdown (0.9.0 feedback). */}
      {run.done && run.feedback && run.feedback !== 'None' && (
        <div className="px-4 pt-0 pb-2">
          <CollapsedPreview label="Verdict" text={run.feedback} popupTitle={`Judge verdict · round ${run.round}`} />
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
          <RunModel run={run} />
          <RunTimer run={run} />
        </summary>
        {run.activity.length > 0 && (
          <div className="px-4 pb-3">
            {running ? <LiveStatusLine activity={run.activity} /> : <ActivityList activity={run.activity} />}
          </div>
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
          placeholder="Retry with guidance (optional) - re-runs this node + downstream…"
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
  onAnswerQuestion?: (nodeId: string, answer: string) => void
}

// Memoized (#421): DagView re-renders on every SSE event for the whole DAG (any
// node's token/tool-call/state change), and an unmemoized DagNode re-ran its full
// body - popup state, timers, activity grouping - for every OTHER node too. The
// callback props are stable (Chat.tsx wraps them in useCallback), so a shallow
// compare bails out for every node except the one that actually changed.
export const DagNode = memo(function DagNode({
  node, state, runs, answer, isFinal,
  onCancel, onPause, onResume, onQueueMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditTask,
  onRetry, onAnswerQuestion,
}: Props) {
  const running = state.status === 'running'
  const notStarted = state.status === 'queued'
  // Retry (→ queued) is legal from done, failed, or cancelled - see dag.CanTransition.
  const finished = state.status === 'done' || state.status === 'failed' || state.status === 'cancelled'
  // The actively-streaming run is the last not-yet-done run while the node runs.
  const activeIdx = running ? runs.map(r => r.done).lastIndexOf(false) : -1
  const [popupOpen, setPopupOpen] = useState(false)
  const pendingQueueCount = (state.queue ?? []).filter(m => !m.delivered).length

  return (
    <div className={`group rounded-xl border shadow-sm overflow-hidden ${
      isFinal
        ? 'border-indigo-200 dark:border-indigo-800 bg-white dark:bg-gray-800'
        : 'border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800'
    }`}>
      {/* Node header */}
      <div className="flex items-center gap-2 px-4 py-3 border-b border-gray-100 dark:border-gray-700">
        <StatusDot status={state.status} />
        <span className="text-xs font-semibold text-gray-700 dark:text-gray-200">
          {agentLabel(node.agent)}
        </span>
        {isAcpAgent(node.agent) && <AcpBadge />}
        <QueuedBadge count={pendingQueueCount} />
        {state.steers && state.steers.length > 0 && (
          <span
            className="text-[10px] font-medium text-amber-600 dark:text-amber-400"
            title={`Queued message(s) delivered:\n${state.steers.join('\n')}`}
          >
            ↻ steered{state.steers.length > 1 ? ` ×${state.steers.length}` : ''}
          </span>
        )}
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
              title={`Judge rejected this output after ${state.judgeRounds} round${state.judgeRounds === 1 ? '' : 's'}${state.judgeFinalScore != null ? ` (final score ${(state.judgeFinalScore * 100).toFixed(0)}%)` : ''} - surfaced unvetted`}
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
          <NodeMenu
            nodeId={node.id}
            status={state.status}
            onCancel={onCancel}
            onPause={onPause}
            onResume={onResume}
            canQueue={running && !!onQueueMessage}
            canEdit={notStarted && !!onEditTask}
            canAnswer={state.status === 'needs_input' && !!onAnswerQuestion}
            onOpenPopup={() => setPopupOpen(true)}
          />
        </div>
      </div>

      {/* Node summary - click to open the popup (#384): the full prompt
          rendered as a chat-native turn, plus (on a live turn) the message
          queue / prompt editor / pending-question answer - one-click
          pause/resume/cancel live in the ⋮ menu above instead. Optimized for
          clean presentation, not information density - the full prompt is
          always recoverable from the trace. */}
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
          onQueueMessage={onQueueMessage}
          onEditQueuedMessage={onEditQueuedMessage}
          onRemoveQueuedMessage={onRemoveQueuedMessage}
          onEditTask={onEditTask}
          onAnswerQuestion={onAnswerQuestion}
        />
      )}

      {/* Retry a finished node (failed or done) + its downstream, on a live turn */}
      {finished && onRetry && (
        <RetryControl nodeId={node.id} onRetry={onRetry} />
      )}

      {/* Per-run stage cards - consecutive worker runs (e.g. a deterministic-
          check retry continuation) merge into one activity feed rather than a
          new boxed block; a judge-triggered revise keeps its own labeled card. */}
      {groupWorkerRuns(runs, activeIdx).map(group => {
        const groupRunning = group.activeIdx >= 0
        switch (group.stage) {
          case 'judge':   return <JudgeCard key={group.runs[0].runId} run={group.runs[0]} running={groupRunning} />
          case 'revise':  return <RevisionCard key={group.runs[0].runId} run={group.runs[0]} running={groupRunning} />
          default:        return <WorkerCard key={group.runs[0].runId} runs={group.runs} running={groupRunning} />
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

      {/* Stopped by the user (node_cancelled) - rendered neutrally, not as an error */}
      {state.status === 'cancelled' && (
        <div className="px-4 py-2 text-xs text-gray-500 dark:text-gray-400 bg-gray-50 dark:bg-gray-800/40">
          Stopped by you
        </div>
      )}
    </div>
  )
})

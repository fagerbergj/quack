import { memo, useEffect, useRef, useState } from 'react'
import { AssistantText, ActivityList, LiveStatusLine, AcpBadge, isAcpAgent } from './AgentParts'
import { ArtifactPanel, isBookkeeping, selectPrimaryOutput, artifactTitle } from './ArtifactPanel'
import { NodeMemoriesPanel } from './NodeMemoriesPanel'
import { CopyButton } from './CopyButton'
import { NodePopup, liveState } from './NodePopup'
import { StatusDot } from './StatusDot'
import { api, listChatArtifactsShared } from '../api'
import type { ArtifactSummary } from '../api'
import type { NodeState, NodeStatus } from '../state/chatStore'
import { agentLabel, type Activity, type AgentRun } from './messageParts'
import { previewLine, fmtTokenCount } from './toolFormat'
import { type DagNodeDef } from '../state/agentStream'
import { fmtMs, LiveTimer } from '../utils/timer'
import { traceUrl } from '../state/clientConfig'
import { Icon } from './Icon'
import { Sheet } from './Sheet'
import { tryParseJSON } from '../lib/json'

// Retry (→ queued) is legal from done, failed, or cancelled - see dag.CanTransition.
const isTerminal = (s: NodeStatus) => s === 'done' || s === 'failed' || s === 'cancelled'

// needs_input is the legacy DB/SSE spelling of paused/awaiting_input - both
// are "paused" for transition purposes (dag.CanTransition treats them alike).
const isPausedStatus = (s: NodeStatus) => s === 'paused' || s === 'needs_input'

// startable/cancellable mirror dag.CanTransition. Raw 'queued' also fires mid-run at a worker/judge
// admission wait, which must read as live, not startable, so the caller supplies live/notStarted.
function menuFlags(status: NodeStatus, live: boolean, notStarted: boolean, canQueue: boolean, canEdit: boolean) {
  const terminal = isTerminal(status)
  const startable = !terminal && (isPausedStatus(status) || notStarted)
  const cancellable = !terminal && (live || startable)
  const hasSecondary = !terminal && (canQueue || canEdit)
  return { startable, cancellable, hasSecondary }
}

// One click for pause/start/stop; "queue a message…" and "edit prompt" open the popup only for its input/editor.
function NodeMenu({
  nodeId, status, live, notStarted, onCancel, onPause, onResume, canQueue, canEdit, onOpenPopup, onOpenArtifacts, onOpenMemories,
}: {
  nodeId: string
  status: NodeStatus
  // live: status === 'running', or 'queued' mid-run (an admission re-wait,
  // not the node's first dispatch). notStarted: 'queued' with no run yet.
  live: boolean
  notStarted: boolean
  onCancel?: (nodeId: string) => void
  onPause?: (nodeId: string) => void
  onResume?: (nodeId: string) => void
  canQueue: boolean
  canEdit: boolean
  onOpenPopup: () => void
  // Present only for a real chat. Not gated by status: a terminal node still needs this menu for its outputs.
  onOpenArtifacts?: () => void
  // Same gating as onOpenArtifacts: a terminal node's received memories are still worth reviewing.
  onOpenMemories?: () => void
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const btnRef = useRef<HTMLButtonElement>(null)
  // The items unmount on close, so without an explicit return a keyboard or
  // screen-reader user is dropped on <body> (APG menu button pattern).
  const close = () => { setOpen(false); btnRef.current?.focus() }

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') close() }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const terminal = isTerminal(status)
  // Viewing outputs usually happens after a node finishes, so hide the menu only when it would be empty.
  if (terminal && !onOpenArtifacts && !onOpenMemories) return null
  const { startable, cancellable, hasSecondary } = menuFlags(status, live, notStarted, canQueue, canEdit)

  return (
    <div ref={ref} className="relative shrink-0">
      {/* Always visible (touch has no hover) and a 44px target; negative margins
          overlap the header padding so the row stays one line high. */}
      <button
        ref={btnRef}
        onClick={() => setOpen(o => !o)}
        aria-label="Node actions"
        aria-haspopup="menu"
        aria-expanded={open}
        className="w-11 h-11 -my-3 -me-3 flex items-center justify-center rounded-lg text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
      >
        <Icon name="more_vert" className="w-5 h-5" />
      </button>
      {open && (
        <NodeMenuItems
          nodeId={nodeId}
          running={live}
          terminal={terminal}
          startable={startable}
          cancellable={cancellable}
          hasSecondary={hasSecondary}
          canQueue={canQueue}
          canEdit={canEdit}
          onPause={onPause}
          onResume={onResume}
          onCancel={onCancel}
          onOpenPopup={onOpenPopup}
          onOpenArtifacts={onOpenArtifacts}
          onOpenMemories={onOpenMemories}
          close={close}
        />
      )}
    </div>
  )
}

// NodeMenu's open state: the per-status items (pause/start/stop, queue a
// message, edit prompt, artifacts, memories), each a 44px menu row.
function NodeMenuItems({ nodeId, running, terminal, startable, cancellable, hasSecondary, canQueue, canEdit,
  onPause, onResume, onCancel, onOpenPopup, onOpenArtifacts, onOpenMemories, close,
}: {
  nodeId: string
  running: boolean
  terminal: boolean
  startable: boolean
  cancellable: boolean
  hasSecondary: boolean
  canQueue: boolean
  canEdit: boolean
  onPause?: (nodeId: string) => void
  onResume?: (nodeId: string) => void
  onCancel?: (nodeId: string) => void
  onOpenPopup: () => void
  onOpenArtifacts?: () => void
  onOpenMemories?: () => void
  close: () => void
}) {
  return (
    <div role="menu" className="absolute z-20 right-0 mt-1 w-48 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg py-1 text-xs">
      {running && onPause && (
        <button role="menuitem" onClick={() => { onPause(nodeId); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700">
          <Icon name="pause" className="w-3.5 h-3.5" /> Pause
        </button>
      )}
      {startable && onResume && (
        <button role="menuitem" onClick={() => { onResume(nodeId); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700">
          <Icon name="play_arrow" className="w-3.5 h-3.5" /> Start
        </button>
      )}
      {cancellable && onCancel && (
        <button role="menuitem" onClick={() => { onCancel(nodeId); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-red-500 dark:text-red-400 hover:bg-gray-50 dark:hover:bg-gray-700">
          <Icon name="stop" className="w-3.5 h-3.5" /> Stop
        </button>
      )}
      {hasSecondary && <div className="my-1 border-t border-gray-100 dark:border-gray-700" />}
      {canQueue && (
        <button role="menuitem" onClick={() => { onOpenPopup(); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700">
          <Icon name="mail" className="w-3.5 h-3.5" /> Queue a message…
        </button>
      )}
      {canEdit && (
        <button role="menuitem" onClick={() => { onOpenPopup(); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700">
          <Icon name="edit" className="w-3.5 h-3.5" /> Edit prompt
        </button>
      )}
      {onOpenArtifacts && (
        <>
          {!terminal && <div className="my-1 border-t border-gray-100 dark:border-gray-700" />}
          <button role="menuitem" onClick={() => { onOpenArtifacts(); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700">
            <Icon name="archive" className="w-3.5 h-3.5" /> Artifacts
          </button>
        </>
      )}
      {onOpenMemories && (
        <button role="menuitem" onClick={() => { onOpenMemories(); close() }} className="w-full text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 flex items-center gap-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700">
          <Icon name="memory" className="w-3.5 h-3.5" /> Memories
        </button>
      )}
    </div>
  )
}

// Counts only parked messages; live-delivered ones don't count.
function QueuedBadge({ count }: { count: number }) {
  if (count === 0) return null
  return (
    <span
      className="text-[11px] font-medium px-1.5 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400 inline-flex items-center gap-0.5"
      title={`${count} parked message${count === 1 ? '' : 's'} - delivers when the current round ends`}
    >
      <Icon name="mail" className="w-3 h-3" /> {count}
    </span>
  )
}

// "by you" needs a real pause_reason (a live-streamed pause may not carry one yet), so the fallback is plain.
export function pausedStatusLabel(status: NodeStatus, reason: NodeState['pauseReason']): string {
  if (status === 'needs_input' || reason === 'awaiting_input') return 'needs your answer'
  switch (reason) {
    case 'user':     return 'paused · by you'
    case 'shutdown': return 'paused · shutdown'
    default:         return 'paused'
  }
}


// RunTimer shows a per-run elapsed timer: live while the run is open, frozen on
// its final duration once complete. Floated right within a card summary.
function RunTimer({ run }: { run: AgentRun }) {
  if (run.done) {
    return run.durationMs != null
      ? <span className="text-[11px] text-gray-500 dark:text-gray-400 tabular-nums ml-auto">{fmtMs(run.durationMs)}</span>
      : null
  }
  return run.startedAt != null
    ? <span className="text-[11px] text-gray-500 dark:text-gray-400 tabular-nums ml-auto"><LiveTimer startedAt={run.startedAt} /></span>
    : null
}

// RunModel shows the model that produced a run, once known (set on agent_complete).
function RunModel({ run }: { run: AgentRun }) {
  if (!run.model) return null
  return (
    <span className="text-[11px] text-gray-500 dark:text-gray-400 font-mono truncate max-w-[100px]" title={run.model}>
      {run.model}
    </span>
  )
}

// CONTEXT_WARN_PCT/CONTEXT_DANGER_PCT: the meter's amber/red thresholds.
const CONTEXT_WARN_PCT = 80
const CONTEXT_DANGER_PCT = 95

// Context-window pressure from the agent's configured limit and the latest worker/revise round's usage.
// Hidden until both are known.
function ContextMeter({ used, limit }: { used: number; limit: number }) {
  if (limit <= 0 || used <= 0) return null
  const pct = Math.min(100, Math.round((used / limit) * 100))
  const barColor = pct >= CONTEXT_DANGER_PCT ? 'bg-red-500 dark:bg-red-400'
    : pct >= CONTEXT_WARN_PCT ? 'bg-amber-500 dark:bg-amber-400'
    : 'bg-gray-400 dark:bg-gray-500'
  const textColor = pct >= CONTEXT_DANGER_PCT ? 'text-red-600 dark:text-red-400'
    : pct >= CONTEXT_WARN_PCT ? 'text-amber-600 dark:text-amber-400'
    : 'text-gray-500 dark:text-gray-400'
  return (
    <span
      className="flex items-center gap-1"
      title={`${used.toLocaleString()} / ${limit.toLocaleString()} tokens of context used`}
    >
      <span className="w-8 h-1 rounded-full bg-gray-200 dark:bg-gray-700 overflow-hidden">
        <span className={`block h-full rounded-full ${barColor}`} style={{ width: `${pct}%` }} />
      </span>
      <span className={`text-[11px] tabular-nums ${textColor}`}>{fmtTokenCount(used)}/{fmtTokenCount(limit)}</span>
    </span>
  )
}

// Shows one block of prose (a judge verdict, a vetted answer) full-size with the same structure as NodePopup:
// light overlay, a close button on its own row, Escape/outside-click to close, a chat-style bubble.
function ContentPopup({ title, text, onClose }: { title: string; text: string; onClose: () => void }) {
  return (
    <Sheet onClose={onClose} className="relative max-w-2xl medium:max-h-[85dvh] medium:rounded-2xl bg-gray-50 dark:bg-gray-900 px-5 medium:pb-6 pt-2 space-y-2">
      <div className="flex justify-end -mb-2">
        <button
          onClick={onClose}
          aria-label="Close"
          className="flex h-11 w-11 -me-3 items-center justify-center rounded-lg text-gray-500 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-400 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
        >
          <Icon name="close" className="w-5 h-5" />
        </button>
      </div>
      <div className="group/verdict relative bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
        <span className="block mb-2 text-[11px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">{title}</span>
        {/* A named group because the popup nests inside DagNode's own `.group` card:
            only this block's hover/focus reveals the copy button. */}
        <span className="absolute top-4 right-5 opacity-0 group-hover/verdict:opacity-100 group-focus-within/verdict:opacity-100 transition-opacity">
          <CopyButton text={text} label={`Copy ${title.toLowerCase()}`} />
        </span>
        <AssistantText text={text} />
      </div>
    </Sheet>
  )
}

// A one-line "label + truncated preview" that opens the full content in a ContentPopup instead of
// expanding inline: a verdict or vetted answer reads better full-size than height-locked in the card.
function CollapsedPreview({ label, text, popupTitle, className = 'py-1 text-[11px]', labelClass = 'italic shrink-0', max }: {
  label: string; text: string; popupTitle: string; className?: string; labelClass?: string; max?: number
}) {
  const [open, setOpen] = useState(false)
  if (!text) return null
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className={`w-full flex items-center gap-1.5 ${className} text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 text-left`}
      >
        <span className={labelClass}>{label}</span>
        <span className="truncate text-gray-500 dark:text-gray-400">{previewLine(text, max)}</span>
      </button>
      {open && <ContentPopup title={popupTitle} text={text} onClose={() => setOpen(false)} />}
    </>
  )
}


// One continuous feed for consecutive worker runs: a mechanical continuation round is a new run but not a stage
// boundary. Memoized so an event re-renders only that run's group.
const WorkerCard = memo(function WorkerCard({ runs, running }: { runs: AgentRun[]; running: boolean }) {
  const activity: Activity[] = runs.length === 1 ? runs[0].activity : runs.flatMap(r => r.activity)
  const empty = activity.length === 0
  if (empty) {
    return running ? <div className="px-4 py-3 text-xs text-gray-500 dark:text-gray-400">starting…</div> : null
  }
  const model = [...runs].reverse().find(r => r.model)?.model
  const startedAt = runs.find(r => r.startedAt != null)?.startedAt
  const durationMs = running ? undefined : runs.reduce((sum, r) => sum + (r.durationMs ?? 0), 0)
  const timerRun: AgentRun = { ...runs[runs.length - 1], startedAt, durationMs, model, done: !running }
  // Count only tool calls: a thinking trace isn't a step, and pure reasoning shows no count, not "0 tool calls".
  const toolCount = activity.filter(a => a.kind === 'tool').length
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          {/* "Running" is already the header's pulsing dot and timer, so no spinner here. */}
          {toolCount > 0 && (
            <span className="text-xs text-gray-500 dark:text-gray-400">
              {`${toolCount} tool call${toolCount === 1 ? '' : 's'}`}
            </span>
          )}
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

// Folds consecutive worker runs into one group so a mechanical continuation merges into its predecessor's feed;
// judge and revise runs mark a real stage and stay their own group.
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

// Parses a fetched revision the same way the panel does: JSON for a
// structured kind, raw text for a blob.
function bodyFor(text: string, klass: string | undefined): unknown {
  return klass === 'structured' ? tryParseJSON(text) ?? text : text
}

// Additive only: the answer keeps rendering at the foot regardless -
// this row shows ONLY when there's an artifact, never substituting for it.
function NodeArtifactSummary({ chatId, nodeId, nodeArtifactKind, finished, onOpen }: {
  chatId: string
  nodeId: string
  nodeArtifactKind?: string
  finished: boolean
  onOpen: () => void
}) {
  const [primary, setPrimary] = useState<ArtifactSummary | null>(null)
  const [body, setBody] = useState<unknown>(undefined)

  useEffect(() => {
    if (!finished) return
    let cancelled = false
    listChatArtifactsShared(chatId).then(l => {
      if (cancelled) return
      const nodeArtifacts = (l.data ?? []).filter(a => a.lineage?.node_id === nodeId && !isBookkeeping(a))
      setPrimary(selectPrimaryOutput(nodeArtifacts, nodeArtifactKind))
    }).catch(() => {})
    return () => { cancelled = true }
  }, [chatId, nodeId, nodeArtifactKind, finished])

  useEffect(() => {
    if (!primary) { setBody(undefined); return }
    let cancelled = false
    api.getArtifactText(chatId, primary.name, primary.latest_revision)
      .then(t => { if (!cancelled) setBody(bodyFor(t, primary.class)) })
      .catch(() => {})
    return () => { cancelled = true }
  }, [chatId, primary])

  if (!primary) return null
  return (
    <button
      type="button"
      onClick={onOpen}
      className="w-full flex items-center gap-1.5 px-4 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 border-t border-gray-100 dark:border-gray-700 text-left"
    >
      <Icon name="archive" className="w-3.5 h-3.5 shrink-0" />
      <span className="truncate">{artifactTitle(primary, body)}</span>
    </button>
  )
}

// "unavailable": the judge model couldn't be reached. "no_verdict": it ran but never committed one,
// which "unavailable" would misreport as an outage.
function judgeFailureHeading(status?: string): string | null {
  if (status === 'unavailable') return 'Judge unavailable'
  if (status === 'no_verdict') return 'Judge did not reach a verdict'
  return null
}

const JudgeCard = memo(function JudgeCard({ run, running }: { run: AgentRun; running: boolean }) {
  const failureHeading = run.done ? judgeFailureHeading(run.status) : null
  if (failureHeading) {
    return (
      <div className="border-t border-gray-100 dark:border-gray-700 px-4 py-2 bg-yellow-50 dark:bg-yellow-900/15">
        <span className="inline-flex items-center gap-1 text-[11px] font-semibold text-yellow-700 dark:text-yellow-400 uppercase tracking-wide">
          <Icon name="warning" className="w-3 h-3" /> {failureHeading} · round {run.round}
        </span>
        <div className="text-[11px] text-yellow-700 dark:text-yellow-400/90 mt-0.5">
          Answer shown without a quality check - {run.reason}
        </div>
      </div>
    )
  }
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={running} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[11px] font-semibold text-purple-600 dark:text-purple-400 uppercase tracking-wide">
            Judge · round {run.round}
          </span>
          {/* The pass bar renders only when the server sent it (older events carry no envelope);
              the threshold lives in the title. */}
          {run.score != null && (
            <span
              className={`inline-flex items-center gap-0.5 text-[11px] font-medium ${run.passed ? 'text-green-600 dark:text-green-400' : 'text-red-500 dark:text-red-400'}`}
              title={`${run.passed ? 'Passed' : 'Failed'}: ${(run.score * 100).toFixed(0)}%${run.threshold != null ? ` (needs ${(run.threshold * 100).toFixed(0)}%)` : ''}`}
            >
              <Icon name={run.passed ? 'check' : 'close'} className="w-3 h-3" /> {(run.score * 100).toFixed(0)}%
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
          <span className="inline-flex items-center gap-1 text-[11px] font-semibold text-blue-600 dark:text-blue-400 uppercase tracking-wide">
            <Icon name="edit" className="w-3 h-3" /> Revised · round {run.round}
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


// Re-runs a finished node and everything downstream. "Retry with guidance" steers the finished node.
// Shown only on a live turn.
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
        <button onClick={() => { setGuiding(false); setText('') }} className="text-[11px] text-gray-500 dark:text-gray-400 hover:underline">cancel</button>
      </div>
    )
  }
  return (
    <div className="flex items-center gap-3 px-4 py-1.5 border-b border-gray-100 dark:border-gray-700">
      <button onClick={() => onRetry(nodeId)} title="Re-run this node and everything downstream of it (reuses the rest)"
        className="inline-flex items-center gap-1 min-h-[44px] medium:min-h-0 text-[11px] font-medium text-indigo-600 dark:text-indigo-400 hover:underline">
        <Icon name="refresh" className="w-3 h-3" /> retry
      </button>
      <button onClick={() => setGuiding(true)} title="Re-run this node with new guidance"
        className="inline-flex items-center gap-1 min-h-[44px] medium:min-h-0 text-[11px] font-medium text-indigo-600 dark:text-indigo-400 hover:underline">
        <Icon name="refresh" className="w-3 h-3" /> retry with guidance…
      </button>
    </div>
  )
}


// The amber "steered" chip (delivered queue messages) and the gray "continues" chip (resumed prior session).
function NodeStateChips({ steers, resumedFrom }: { steers?: string[]; resumedFrom?: string }) {
  return (
    <>
      {steers && steers.length > 0 && (
        <span
          className="shrink-0 inline-flex items-center gap-0.5 text-[11px] font-medium text-amber-600 dark:text-amber-400"
          title={`Queued message(s) delivered:\n${steers.join('\n')}`}
        >
          <Icon name="mail" className="w-3 h-3" /> steered{steers.length > 1 ? ` ×${steers.length}` : ''}
        </span>
      )}
      {resumedFrom && (
        <span
          className="shrink-0 inline-flex items-center gap-0.5 text-[11px] font-medium text-gray-500 dark:text-gray-400"
          title={`This node picked up where it left off, on its own prior session (${resumedFrom})`}
        >
          <Icon name="history" className="w-3 h-3" /> continues
        </span>
      )}
    </>
  )
}

// The named state leads the metadata group so below `medium` it wraps onto the muted second line
// instead of squeezing the agent name off the first.
function NodeMetaRow({ state, node, pauseLabel }: { state: NodeState; node: DagNodeDef; pauseLabel?: string }) {
  return (
    <div className="empty:hidden flex flex-wrap medium:flex-nowrap items-center gap-x-2 gap-y-0.5 basis-full order-last medium:basis-auto medium:order-none">
      <StatusDot status={state.status} label={pauseLabel} />
      {state.model && (
        <span className="text-[11px] text-gray-500 dark:text-gray-400 font-mono truncate max-w-[120px]" title={state.model}>
          {state.model}
        </span>
      )}
      {state.finishReason === 'MAX_TOKENS' && (
        <span className="text-[11px] font-medium text-amber-600 dark:text-amber-400" title="Response was truncated at the token limit">
          truncated
        </span>
      )}
      {state.judgeRounds != null && state.judgeRounds > 0 && state.judgePassed === false && (
        <span
          className="inline-flex items-center gap-0.5 text-[11px] font-medium text-amber-600 dark:text-amber-400"
          title={`The quality check rejected this output after ${state.judgeRounds} round${state.judgeRounds === 1 ? '' : 's'}${state.judgeFinalScore != null ? ` (final score ${(state.judgeFinalScore * 100).toFixed(0)}%)` : ''} - shown without a passing check`}
        >
          <Icon name="warning" className="w-3 h-3" /> not checked
        </span>
      )}
      {state.totalTokens != null && state.totalTokens > 0 && (
        <span className="text-[11px] text-gray-500 dark:text-gray-400 tabular-nums">
          {state.totalTokens.toLocaleString()} tokens
          {state.cachedTokens != null && state.cachedTokens > 0 && (
            <span title={`${state.cachedTokens.toLocaleString()} tokens served from cache`}> ({state.cachedTokens.toLocaleString()} cached)</span>
          )}
        </span>
      )}
      {traceUrl(state.traceId) && (
        <a
          href={traceUrl(state.traceId)}
          target="_blank"
          rel="noreferrer"
          className="text-[11px] text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 underline"
          title="Open this run's trace in the tracing backend (whole run, not just this node)"
        >
          run trace
        </a>
      )}
      <ContextMeter used={state.contextTokens ?? 0} limit={node.context_window ?? 0} />
    </div>
  )
}

// A finished node shows the server-measured duration (reconnect-proof); a running one ticks from the server start.
function NodeElapsed({ state }: { state: NodeState }) {
  return (state.finishedAt != null && state.serverDurationMs != null) ? (
    <span className="shrink-0 text-[11px] text-gray-500 dark:text-gray-400 tabular-nums">{fmtMs(state.serverDurationMs)}</span>
  ) : state.startedAt != null ? (
    <span className="shrink-0 text-[11px] text-gray-500 dark:text-gray-400 tabular-nums">
      <LiveTimer startedAt={state.startedAt} finishedAt={state.finishedAt} />
    </span>
  ) : null
}

// Four narrow fields, not the whole NodeState, so the panels skip re-rendering on every SSE event for the node.
// nodeError matches the failed-status banner, so a transient error never reads as a failure in an empty panel.
function SidePanels({ chatId, node, state, answer, artifactsOpen, memoriesOpen, onCloseArtifacts, onCloseMemories }: {
  chatId: string
  node: DagNodeDef
  state: NodeState
  answer: string
  artifactsOpen: boolean
  memoriesOpen: boolean
  onCloseArtifacts: () => void
  onCloseMemories: () => void
}) {
  return (
    <>
      {artifactsOpen && (
        <ArtifactPanel
          chatId={chatId}
          nodeId={node.id}
          nodeAgent={agentLabel(node.agent)}
          nodeTask={node.task}
          nodeError={state.status === 'failed' && state.error ? state.error : undefined}
          nodeAnswer={answer}
          nodeArtifactKind={node.artifact ?? undefined}
          onClose={onCloseArtifacts}
        />
      )}
      {memoriesOpen && (
        <NodeMemoriesPanel
          chatId={chatId}
          nodeId={node.id}
          judgeRounds={state.judgeRounds}
          onClose={onCloseMemories}
        />
      )}
    </>
  )
}

// RunGroupList: the per-run stage cards, grouped (groupWorkerRuns).
function RunGroupList({ runs, activeIdx }: { runs: AgentRun[]; activeIdx: number }) {
  return (
    <>
      {groupWorkerRuns(runs, activeIdx).map(group => {
        const groupRunning = group.activeIdx >= 0
        switch (group.stage) {
          case 'judge':   return <JudgeCard key={group.runs[0].runId} run={group.runs[0]} running={groupRunning} />
          case 'revise':  return <RevisionCard key={group.runs[0].runId} run={group.runs[0]} running={groupRunning} />
          default:        return <WorkerCard key={group.runs[0].runId} runs={group.runs} running={groupRunning} />
        }
      })}
    </>
  )
}

// StatusBanners: the footer state banners under the vetted answer.
function StatusBanners({ state }: { state: NodeState }) {
  return (
    <>
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
    </>
  )
}


// A node blocked on the user is the one state where the fix is the primary action, so a filled button, not a menu item.
function NodeAnswerButton({ canAnswer, onClick }: { canAnswer: boolean; onClick: () => void }) {
  if (!canAnswer) return null
  return (
    <button
      type="button"
      onClick={onClick}
      className="shrink-0 h-11 -my-3 px-3 inline-flex items-center gap-1 rounded-lg bg-amber-600 text-white text-xs font-medium hover:bg-amber-700 transition-colors"
    >
      <Icon name="help" className="w-3.5 h-3.5" /> Answer
    </button>
  )
}

// RetryGate: RetryControl on a live turn, for a finished node only.
function RetryGate({ nodeId, finished, onRetry }: {
  nodeId: string
  finished: boolean
  onRetry?: (nodeId: string, guidance?: string) => void
}) {
  if (!finished || !onRetry) return null
  return <RetryControl nodeId={nodeId} onRetry={onRetry} />
}


// OutcomeRow: the vetted answer ALWAYS renders; a real chat additionally
// gets the artifact summary above it. Split out to keep DagNode's own complexity down.
function OutcomeRow({ chatId, node, answer, finished, onOpenArtifacts }: {
  chatId: string | undefined
  node: DagNodeDef
  answer: string
  finished: boolean
  onOpenArtifacts: () => void
}) {
  return (
    <>
      {chatId && (
        <NodeArtifactSummary
          chatId={chatId}
          nodeId={node.id}
          nodeArtifactKind={node.artifact ?? undefined}
          finished={finished}
          onOpen={onOpenArtifacts}
        />
      )}
      <CollapsedPreview label="answer" text={answer} popupTitle="Answer" max={100} labelClass="shrink-0"
        className="px-4 py-2 text-xs border-t border-gray-100 dark:border-gray-700" />
    </>
  )
}

interface Props {
  node: DagNodeDef
  state: NodeState
  runs: AgentRun[]
  answer: string
  isFinal: boolean
  // Present only for a real chat (not a Storybook fixture): the Artifacts panel needs it for the REST API.
  chatId?: string
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

// Memoized because DagView re-renders on every SSE event for the whole DAG; callback props are stable
// (useCallback in Chat.tsx), so a shallow compare skips every node but the changed one.
export const DagNode = memo(function DagNode({
  node, state, runs, answer, isFinal, chatId,
  onCancel, onPause, onResume, onQueueMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditTask,
  onRetry, onAnswerQuestion,
}: Props) {
  const dispatched = runs.length > 0
  const { notStarted, running } = liveState(state.status, dispatched)
  const finished = isTerminal(state.status)
  // The actively-streaming run is the last not-yet-done run while the node runs.
  const activeIdx = running ? runs.map(r => r.done).lastIndexOf(false) : -1
  const [popupOpen, setPopupOpen] = useState(false)
  const [artifactsOpen, setArtifactsOpen] = useState(false)
  const [memoriesOpen, setMemoriesOpen] = useState(false)
  const pendingQueueCount = (state.queue ?? []).filter(m => !m.delivered).length
  const isPaused = isPausedStatus(state.status)
  const pauseLabel = isPaused ? pausedStatusLabel(state.status, state.pauseReason) : undefined
  const canAnswer = (state.status === 'needs_input' || state.pauseReason === 'awaiting_input') && !!onAnswerQuestion

  // overflow-hidden clips the rounded corners but also clipped the kebab menu to a short card,
  // so it lifts while the menu is open.
  return (
    <div className={`rounded-xl border shadow-sm overflow-hidden has-[[aria-expanded=true]]:overflow-visible ${
      isFinal
        ? 'border-indigo-200 dark:border-indigo-800 bg-white dark:bg-gray-800'
        : 'border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800'
    }`}>
      {/* One line at every width; below `medium` the metadata group wraps onto its own
          muted line (basis-full + order-last) instead of stacking the row. */}
      <div className="flex flex-wrap medium:flex-nowrap items-center gap-x-2 gap-y-1 px-4 py-3 border-b border-gray-100 dark:border-gray-700">
        <span className="text-xs font-semibold text-gray-700 dark:text-gray-200 min-w-0 flex-1 truncate" title={agentLabel(node.agent)}>
          {agentLabel(node.agent)}
        </span>
        {isAcpAgent(node.agent) && <AcpBadge />}
        <QueuedBadge count={pendingQueueCount} />
        <NodeStateChips steers={state.steers} resumedFrom={state.resumedFrom} />
        <NodeMetaRow state={state} node={node} pauseLabel={pauseLabel} />
        <NodeElapsed state={state} />
        <NodeAnswerButton canAnswer={canAnswer} onClick={() => setPopupOpen(true)} />
        <NodeMenu
          nodeId={node.id}
          status={state.status}
          live={running}
          notStarted={notStarted}
          onCancel={onCancel}
          onPause={onPause}
          onResume={onResume}
          canQueue={running && !!onQueueMessage}
          canEdit={notStarted && !!onEditTask}
          onOpenPopup={() => setPopupOpen(true)}
          onOpenArtifacts={chatId ? () => setArtifactsOpen(true) : undefined}
          onOpenMemories={chatId ? () => setMemoriesOpen(true) : undefined}
        />
      </div>

      {/* Opens the popup: the full prompt as a chat-native turn plus, on a live turn, the message
          queue, prompt editor and pending-question answer. */}
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
          dispatched={dispatched}
          onClose={() => setPopupOpen(false)}
          onQueueMessage={onQueueMessage}
          onEditQueuedMessage={onEditQueuedMessage}
          onRemoveQueuedMessage={onRemoveQueuedMessage}
          onEditTask={onEditTask}
          onAnswerQuestion={onAnswerQuestion}
        />
      )}
      {chatId && (
        <SidePanels
          chatId={chatId}
          node={node}
          state={state}
          answer={answer}
          artifactsOpen={artifactsOpen}
          memoriesOpen={memoriesOpen}
          onCloseArtifacts={() => setArtifactsOpen(false)}
          onCloseMemories={() => setMemoriesOpen(false)}
        />
      )}

      {/* Retry a finished node (failed or done) + its downstream, on a live turn */}
      <RetryGate nodeId={node.id} finished={finished} onRetry={onRetry} />

      {/* Consecutive worker runs merge into one feed; a judge-triggered revise keeps its own labeled card. */}
      <RunGroupList runs={runs} activeIdx={activeIdx} />

      {/* The outcome summary (below the stage cards, for every node). */}
      {!isFinal && (
        <OutcomeRow chatId={chatId} node={node} answer={answer} finished={finished} onOpenArtifacts={() => setArtifactsOpen(true)} />
      )}

      <StatusBanners state={state} />
    </div>
  )
})

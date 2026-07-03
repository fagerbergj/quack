import { useState } from 'react'
import { AssistantText, ActivityList } from './AgentParts'
import type { NodeState, NodeStatus } from '../state/chatStore'
import type { AgentRun } from './messageParts'
import { CANCELLED_ERROR, type DagNodeDef } from '../state/agentStream'
import { fmtMs, LiveTimer } from '../utils/timer'

function StatusBadge({ status, stopped }: { status: NodeStatus; stopped?: boolean }) {
  if (stopped) {
    return (
      <span className="text-[10px] font-medium px-1.5 py-0.5 rounded bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300">
        stopped
      </span>
    )
  }
  const styles: Record<NodeStatus, string> = {
    queued:  'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400',
    running: 'bg-blue-100 text-blue-600 dark:bg-blue-900/40 dark:text-blue-400',
    needs_input: 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400',
    done:    'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400',
    failed:  'bg-red-100 text-red-600 dark:bg-red-900/40 dark:text-red-400',
  }
  const labels: Record<NodeStatus, string> = {
    queued: 'queued', running: 'running…', done: 'done', failed: 'failed',
    needs_input: 'waiting for you',
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

// ResearchCard renders the worker stage (draft + finalize): its activity. The
// node's vetted answer is rendered separately at the foot of the node (NodeAnswer),
// so it sits below the judge rather than inside the worker card.
function ResearchCard({ run, running }: { run: AgentRun; running: boolean }) {
  const empty = run.activity.length === 0
  if (empty) {
    return running ? <div className="px-4 py-3 text-xs text-gray-400 dark:text-gray-500">starting…</div> : null
  }
  return (
    <details open={running} className="not-prose">
      <summary className="cursor-pointer select-none px-4 py-2 text-xs text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300">
        {running ? 'activity…' : `${run.activity.length} step${run.activity.length === 1 ? '' : 's'}`}
      </summary>
      <div className="px-4 pb-3">
        <ActivityList activity={run.activity} />
      </div>
    </details>
  )
}

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
        <AssistantText text={answer} />
      </div>
    </details>
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

// NodeControls is the live per-node control row (stop / steer), shown only while
// a node is running or queued and only on a live run (callbacks present). Steering
// reveals an inline input; the guidance interrupts the node and re-runs it with
// its prior work intact.
function NodeControls({ nodeId, onStop, onSteer }: {
  nodeId: string
  onStop?: (nodeId: string) => void
  onSteer?: (nodeId: string, guidance: string) => void
}) {
  const [steering, setSteering] = useState(false)
  const [text, setText] = useState('')
  if (!onStop && !onSteer) return null
  const send = () => {
    const g = text.trim()
    if (!g) return
    onSteer?.(nodeId, g)
    setText('')
    setSteering(false)
  }
  if (steering) {
    return (
      <div className="flex items-center gap-2 px-4 py-2 border-b border-gray-100 dark:border-gray-700 bg-amber-50/50 dark:bg-amber-900/10">
        <input
          autoFocus
          value={text}
          onChange={e => setText(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') { e.preventDefault(); send() }
            if (e.key === 'Escape') { setSteering(false); setText('') }
          }}
          placeholder="Steer this node — new guidance (keeps its work)…"
          className="flex-1 min-w-0 text-xs px-2 py-1 rounded border border-amber-300 dark:border-amber-700 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-amber-400"
        />
        <button onClick={send} className="text-[11px] font-medium text-amber-700 dark:text-amber-400 hover:underline">send</button>
        <button onClick={() => { setSteering(false); setText('') }} className="text-[11px] text-gray-400 dark:text-gray-500 hover:underline">cancel</button>
      </div>
    )
  }
  return (
    <div className="flex items-center gap-3 px-4 py-1.5 border-b border-gray-100 dark:border-gray-700">
      {onSteer && (
        <button onClick={() => setSteering(true)} title="Interrupt and re-run this node with new guidance (keeps its work)"
          className="text-[10px] font-medium text-amber-600 dark:text-amber-400 hover:underline">
          ↻ steer
        </button>
      )}
      {onStop && (
        <button onClick={() => onStop(nodeId)} title="Stop this node (the rest of the run continues)"
          className="text-[10px] font-medium text-red-500 dark:text-red-400 hover:underline">
          ✕ stop
        </button>
      )}
    </div>
  )
}

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

// NodeAskPrompt renders a paused node's question (mid-node HITL) with an inline
// answer box. The answer is sent as the next chat message — the backend routes it
// to the paused node (see orchestrator resumeNodeRun).
function NodeAskPrompt({ question, onAnswer }: {
  question: string
  onAnswer: (answer: string) => void
}) {
  const [text, setText] = useState('')
  const send = () => {
    const t = text.trim()
    if (!t) return
    onAnswer(t)
    setText('')
  }
  return (
    <div className="px-4 py-2 border-b border-amber-200 dark:border-amber-800 bg-amber-50/70 dark:bg-amber-900/15">
      <div className="text-xs font-medium text-amber-800 dark:text-amber-300 mb-1.5">
        This agent needs your input: <span className="font-normal">{question}</span>
      </div>
      <div className="flex items-center gap-2">
        <input
          autoFocus
          value={text}
          onChange={e => setText(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); send() } }}
          placeholder="Type your answer…"
          className="flex-1 min-w-0 text-xs px-2 py-1 rounded border border-amber-300 dark:border-amber-700 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-amber-400"
        />
        <button onClick={send} className="text-[11px] font-medium text-amber-700 dark:text-amber-400 hover:underline">answer</button>
      </div>
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
  onStop?: (nodeId: string) => void
  onSteer?: (nodeId: string, guidance: string) => void
  onRetry?: (nodeId: string, guidance?: string) => void
  // Answer a paused node's question (mid-node HITL); present on a live turn.
  onAnswer?: (nodeId: string, answer: string) => void
}

export function DagNode({ node, state, runs, answer, isFinal, onStop, onSteer, onRetry, onAnswer }: Props) {
  const running = state.status === 'running'
  const controllable = running || state.status === 'queued'
  const finished = state.status === 'done' || state.status === 'failed'
  // A user-cancelled node comes back as failed with this specific error; render it
  // as a neutral "stopped" rather than a red failure.
  const stopped = state.status === 'failed' && state.error === CANCELLED_ERROR
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
        <StatusBadge status={state.status} stopped={stopped} />
        {state.steers && state.steers.length > 0 && (
          <span
            className="text-[10px] font-medium text-amber-600 dark:text-amber-400"
            title={`Steered:\n${state.steers.join('\n')}`}
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
          {(state.totalTokens != null && state.totalTokens > 0)
            ? <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">{state.totalTokens.toLocaleString()} tok</span>
            : state.outputChars != null && state.outputChars > 0
              ? <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">~{Math.round(state.outputChars / 4).toLocaleString()} tok</span>
              : null
          }
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

      {/* Task description */}
      <div className="px-4 py-2 text-xs text-gray-500 dark:text-gray-400 border-b border-gray-100 dark:border-gray-700">
        {node.task}
      </div>

      {/* Live per-node controls (stop / steer) — only while running/queued */}
      {controllable && (onStop || onSteer) && (
        <NodeControls nodeId={node.id} onStop={onStop} onSteer={onSteer} />
      )}

      {/* Retry a finished node (failed or done) + its downstream, on a live turn */}
      {finished && onRetry && (
        <RetryControl nodeId={node.id} onRetry={onRetry} />
      )}

      {/* Mid-node HITL: the node paused to ask the user a question */}
      {state.status === 'needs_input' && state.question && onAnswer && (
        <NodeAskPrompt question={state.question} onAnswer={a => onAnswer(node.id, a)} />
      )}

      {/* Per-run stage cards */}
      {runs.map((run, i) => {
        const runRunning = i === activeIdx
        switch (run.stage) {
          case 'self_refine': return <SelfCritiqueCard key={run.runId} run={run} running={runRunning} />
          case 'judge':       return <JudgeCard key={run.runId} run={run} running={runRunning} />
          case 'revise':      return <RevisionCard key={run.runId} run={run} running={runRunning} />
          default:            return <ResearchCard key={run.runId} run={run} running={runRunning} />
        }
      })}

      {/* Vetted answer (below the stage cards, for every node) */}
      <NodeAnswer answer={answer} />

      {/* Failed / stopped state (a user-cancelled node reads neutrally, not as an error) */}
      {state.status === 'failed' && state.error && (
        <div className={`px-4 py-2 text-xs ${stopped
          ? 'text-gray-500 dark:text-gray-400 bg-gray-50 dark:bg-gray-800/40'
          : 'text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/20'}`}>
          {state.error}
        </div>
      )}
    </div>
  )
}

import { AssistantParts, WindowedItems } from './AgentParts'
import type { NodeState, NodeStatus } from '../state/chatStore'
import type { MessagePart, SelfRefinePart, JudgeVerdictPart } from './messageParts'
import type { DagNodeDef } from '../state/agentStream'

function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

function elapsed(startedAt: number, finishedAt?: number): string {
  return fmtMs((finishedAt ?? Date.now()) - startedAt)
}

// ── gate types ──────────────────────────────────────────────────────────────

type ResearchGate  = { kind: 'research';    parts: MessagePart[] }
type SelfCritGate  = { kind: 'self-refine'; part: SelfRefinePart }
type JudgeGate     = { kind: 'judge';       part: JudgeVerdictPart }
type Gate = ResearchGate | SelfCritGate | JudgeGate

// splitGates groups flat top-level parts into sequential phase cards.
// revise / judge_unavailable parts are implicit connectors; they don't get
// their own gate but the content that follows them (revised answer, next judge
// round) naturally appears in the next research / judge gate.
function splitGates(parts: MessagePart[]): Gate[] {
  const gates: Gate[] = []
  let buf: MessagePart[] = []

  const flushBuf = () => {
    if (buf.length > 0) {
      gates.push({ kind: 'research', parts: buf })
      buf = []
    }
  }

  for (const p of parts) {
    if (p.kind === 'self_refine') {
      flushBuf()
      gates.push({ kind: 'self-refine', part: p })
    } else if (p.kind === 'judge_verdict') {
      flushBuf()
      gates.push({ kind: 'judge', part: p })
    } else if (p.kind === 'revise' || p.kind === 'judge_unavailable') {
      // Implicit — no gate, just flush so subsequent content starts fresh
      flushBuf()
    } else {
      buf.push(p)
    }
  }
  flushBuf()

  // Always return at least an empty research gate so the node body exists
  if (gates.length === 0) gates.push({ kind: 'research', parts: [] })
  return gates
}

// ── shared sub-components ───────────────────────────────────────────────────

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

function agentLabel(name: string): string {
  if (name === 'web-researcher') return 'Web researcher'
  if (name === 'synthesizer') return 'Synthesizer'
  return name
}

// ── gate card renderers ─────────────────────────────────────────────────────

function ResearchCard({ gate, running, expand }: { gate: ResearchGate; running: boolean; expand: boolean }) {
  if (gate.parts.length === 0) {
    return running ? (
      <div className="px-4 py-3 text-xs text-gray-400 dark:text-gray-500">starting…</div>
    ) : null
  }
  if (expand) {
    return (
      <div className="px-4 py-3">
        <AssistantParts parts={gate.parts} showCursor={running} />
      </div>
    )
  }
  return (
    <details open={running} className="not-prose">
      <summary className="cursor-pointer select-none px-4 py-2 text-xs text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300">
        {running ? 'activity…' : `${gate.parts.length} step${gate.parts.length === 1 ? '' : 's'}`}
      </summary>
      <div className="px-4 pb-3">
        <WindowedItems items={gate.parts} />
      </div>
    </details>
  )
}

function SelfCritiqueCard({ gate, running }: { gate: SelfCritGate; running: boolean }) {
  const isRunning = running && !gate.part.done
  const changed = gate.part.changed
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={isRunning} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-amber-600 dark:text-amber-400 uppercase tracking-wide">
            Self-critique
          </span>
          {isRunning && <Spinner />}
          {gate.part.done && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500">
              {changed ? 'revised' : 'no changes'}
            </span>
          )}
          {gate.part.done && gate.part.durationMs != null && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums ml-auto">{fmtMs(gate.part.durationMs)}</span>
          )}
        </summary>
        {gate.part.items.length > 0 && (
          <div className="px-4 pb-3">
            <WindowedItems items={gate.part.items} />
          </div>
        )}
      </details>
    </div>
  )
}

function JudgeCard({ gate, running }: { gate: JudgeGate; running: boolean }) {
  const isRunning = running && !gate.part.done
  const { round, score, passed, feedback, items } = gate.part
  return (
    <div className="border-t border-gray-100 dark:border-gray-700">
      <details open={isRunning} className="not-prose">
        <summary className="cursor-pointer select-none px-4 py-2 flex items-center gap-2">
          <span className="text-[10px] font-semibold text-purple-600 dark:text-purple-400 uppercase tracking-wide">
            Judge · round {round}
          </span>
          {isRunning && <Spinner />}
          {gate.part.done && score != null && (
            <span className={`text-[10px] font-medium ${passed ? 'text-green-600 dark:text-green-400' : 'text-red-500 dark:text-red-400'}`}>
              {passed ? '✓' : '✗'} {(score * 100).toFixed(0)}%
            </span>
          )}
          {gate.part.done && feedback && !passed && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 truncate max-w-[200px]">
              — {feedback}
            </span>
          )}
          {gate.part.done && gate.part.durationMs != null && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums ml-auto">{fmtMs(gate.part.durationMs)}</span>
          )}
        </summary>
        {items.length > 0 && (
          <div className="px-4 pb-3">
            <WindowedItems items={items} />
          </div>
        )}
      </details>
    </div>
  )
}

// ── DagNode ─────────────────────────────────────────────────────────────────

interface Props {
  node: DagNodeDef
  state: NodeState
  parts: MessagePart[]
  isFinal: boolean
}

export function DagNode({ node, state, parts, isFinal }: Props) {
  const running = state.status === 'running'
  const gates = splitGates(parts)

  // Which gate is actively streaming (the last one when running)
  const activeGateIdx = running ? gates.length - 1 : -1

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
        {running && gates.every(g => g.kind === 'research' && g.parts.length === 0) && (
          <Spinner />
        )}
        <div className="ml-auto flex items-center gap-2">
          {state.outputChars != null && state.outputChars > 0 && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
              ~{Math.round(state.outputChars / 4).toLocaleString()} tok
            </span>
          )}
          {state.startedAt != null && (
            <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
              {elapsed(state.startedAt, state.finishedAt)}
            </span>
          )}
        </div>
      </div>

      {/* Task description */}
      <div className="px-4 py-2 text-xs text-gray-500 dark:text-gray-400 border-b border-gray-100 dark:border-gray-700">
        {node.task}
      </div>

      {/* Gate cards */}
      {gates.map((gate, i) => {
        const gateRunning = running && i === activeGateIdx
        if (gate.kind === 'research') {
          return <ResearchCard key={i} gate={gate} running={gateRunning} expand={isFinal} />
        }
        if (gate.kind === 'self-refine') {
          return <SelfCritiqueCard key={i} gate={gate} running={gateRunning} />
        }
        return <JudgeCard key={i} gate={gate} running={gateRunning} />
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

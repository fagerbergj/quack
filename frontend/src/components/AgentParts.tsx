import { useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { ReactNode } from 'react'
import type { MessagePart, ToolCallPart, AgentPart, SelfRefinePart, RevisePart, JudgeVerdictPart, JudgeUnavailablePart } from './messageParts'
import { summarizeArgs, prettyJSON } from './toolFormat'

// Re-export the data layer so existing import sites (`from '.../AgentParts'`)
// keep working; the types and reducers live in messageParts.ts.
export * from './messageParts'

// RECENT is how many of an actor's most recent items stay visible; older ones fold
// into a "⋯ N earlier" toggle so a long run stays scannable.
const RECENT = 3

// AssistantParts renders the message tree: answer/narrative text at the top level,
// and actor groups (with their nested activity) as collapsible, windowed blocks.
export function AssistantParts({ parts, showCursor }: {
  parts: MessagePart[]
  showCursor?: boolean
}) {
  const last = parts[parts.length - 1]
  const cursorAfterText = showCursor && last?.kind === 'text'
  return (
    <>
      {parts.map((p, i) => <PartView key={i} part={p} />)}
      {cursorAfterText && (
        <span className="inline-block w-1.5 h-4 bg-gray-400 animate-pulse ml-0.5 align-middle" />
      )}
    </>
  )
}

// PartView dispatches one tree node to its renderer.
function PartView({ part }: { part: MessagePart }) {
  switch (part.kind) {
    case 'text': return <AssistantText text={part.text} />
    case 'thinking': return <ThinkBlock text={part.text} />
    case 'tool_call': return <ToolBlock part={part} />
    case 'agent': return <AgentGroup part={part} />
    case 'self_refine': return <SelfRefineGroup part={part} />
    case 'revise': return <ReviseBlock part={part} />
    case 'judge_verdict': return <JudgeGroup part={part} />
    case 'judge_unavailable': return <JudgeUnavailableBlock part={part} />
  }
}

// AssistantText renders model text as markdown.
export function AssistantText({ text }: { text: string }) {
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  )
}

// displayName maps internal agent names to human-readable labels.
function displayName(agent: string): string {
  return agent === 'orchestrator' ? 'Quack' : agent
}

// AgentGroup renders one actor's container: a "→ agent" header plus its activity
// (thinking / tool calls / nested agents), windowed to the most recent few.
// Finished groups start collapsed so completed runs stay scannable.
function AgentGroup({ part }: { part: AgentPart }) {
  const running = !part.done
  return (
    <details open={running} className="my-2 rounded-lg border border-indigo-200 dark:border-indigo-900 bg-indigo-50/50 dark:bg-indigo-950/30 not-prose">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
        <span className="font-medium text-indigo-600 dark:text-indigo-400">→ {displayName(part.agent)}</span>
        {running ? <Dots /> : (
          <span className="text-gray-400 dark:text-gray-500">
            {part.items.length} step{part.items.length === 1 ? '' : 's'}
          </span>
        )}
      </summary>
      <div className="pl-3 pr-2 pb-2 ml-2 border-l border-indigo-200 dark:border-indigo-900">
        <WindowedItems items={part.items} />
        {running && part.items.length === 0 && (
          <div className="px-1 py-2 text-xs text-gray-400 dark:text-gray-500">starting…</div>
        )}
      </div>
    </details>
  )
}

// SelfRefineGroup is a collapsible container for a self-refine pass. While
// running it shows a spinner; when done it shows whether the draft was revised.
// Finished passes auto-collapse so the reader can focus on the judge.
function SelfRefineGroup({ part }: { part: SelfRefinePart }) {
  const running = !part.done
  const label = running
    ? 'self-critique…'
    : part.changed ? 'self-critique — revised' : 'self-critique — no changes'
  return (
    <details open={running} className="my-2 rounded-lg border border-purple-200 dark:border-purple-900 bg-purple-50/50 dark:bg-purple-950/20 not-prose">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
        <span className="font-medium text-purple-600 dark:text-purple-400">✎ {label}</span>
        {running && <Dots />}
      </summary>
      {part.items.length > 0 && (
        <div className="pl-3 pr-2 pb-2 ml-2 border-l border-purple-200 dark:border-purple-900">
          {part.items.map((it, i) => <PartView key={i} part={it} />)}
        </div>
      )}
    </details>
  )
}

// JudgeGroup is a collapsible container for one judge round. While running it
// shows a spinner; when done it shows the pass/fail badge with score.
// Finished rounds auto-collapse so only the most recent is open.
function JudgeGroup({ part }: { part: JudgeVerdictPart }) {
  const running = !part.done
  const pct = part.score !== undefined ? Math.round(part.score * 100) : undefined
  const badge = !running && (
    <span className={`rounded px-1.5 py-0.5 font-medium ${part.passed
      ? 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400'
      : 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400'
    }`}>
      {part.passed ? 'pass' : 'revise'} · {pct}%
    </span>
  )
  return (
    <div className="my-2 not-prose">
      <details open={running} className="rounded-lg border border-gray-200 dark:border-gray-700 bg-gray-50/50 dark:bg-gray-900/30">
        <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
          <span className="text-gray-500 dark:text-gray-400">⚖ judge · round {part.round}</span>
          {running ? <Dots /> : badge}
        </summary>
        {part.items.length > 0 && (
          <div className="pl-3 pr-2 pb-2 ml-2 border-l border-gray-200 dark:border-gray-700">
            {part.items.map((it, i) => <PartView key={i} part={it} />)}
          </div>
        )}
      </details>
      {!running && part.feedback && part.feedback !== 'None' && (
        <div className="mt-1 px-1 text-xs text-gray-500 dark:text-gray-400">{part.feedback}</div>
      )}
    </div>
  )
}

// ReviseBlock is a one-line note that the gate revised the answer after a
// judge fail, shown between the failing and passing verdict rows.
function ReviseBlock({ part }: { part: RevisePart }) {
  return (
    <div className="my-1.5 px-1 text-xs text-gray-500 dark:text-gray-400 not-prose">
      ↺ revised · round {part.round}
    </div>
  )
}

// JudgeUnavailableBlock renders a warning banner when the judge failed and the
// answer is surfaced without a quality guarantee.
function JudgeUnavailableBlock({ part }: { part: JudgeUnavailablePart }) {
  return (
    <div className="my-1.5 px-2 py-1.5 rounded text-xs not-prose bg-yellow-50 dark:bg-yellow-900/20 border border-yellow-300 dark:border-yellow-700">
      <span className="font-medium text-yellow-700 dark:text-yellow-400">⚠ Quality cannot be guaranteed</span>
      <span className="text-yellow-600 dark:text-yellow-500"> — judge was unavailable (round {part.round})</span>
    </div>
  )
}

// WindowedItems shows the most recent RECENT items; older ones collapse behind a
// "⋯ N earlier" toggle. Keys index into the full list (which only ever appends),
// so streaming reconciliation stays stable.
function WindowedItems({ items }: { items: MessagePart[] }) {
  const [showAll, setShowAll] = useState(false)
  const hidden = Math.max(0, items.length - RECENT)
  const start = showAll ? 0 : hidden
  return (
    <>
      {hidden > 0 && (
        <button
          onClick={() => setShowAll(s => !s)}
          className="my-1 text-[11px] text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
        >
          {showAll ? '▾ show less' : `⋯ ${hidden} earlier`}
        </button>
      )}
      {items.slice(start).map((it, i) => <PartView key={start + i} part={it} />)}
    </>
  )
}

// ThinkBlock renders reasoning (the `thinking` event) as a collapsed block.
function ThinkBlock({ text }: { text: string }) {
  return (
    <details className="my-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-gray-50 dark:bg-gray-900/40 not-prose">
      <summary className="cursor-pointer select-none px-3 py-1.5 text-xs font-medium text-gray-500 dark:text-gray-400">
        thinking
      </summary>
      <div className="px-3 pb-3 pt-1 text-xs text-gray-600 dark:text-gray-300 whitespace-pre-wrap font-mono">
        {text}
      </div>
    </details>
  )
}

// ToolBlock renders a tool call as a collapsed block: name + args summary;
// expanded shows args and the result (or "running…" until it returns).
export function ToolBlock({ part }: { part: ToolCallPart }) {
  const argSummary = summarizeArgs(part.args)
  const isRunning = part.result === undefined
  return (
    <details className="my-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 not-prose">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
        <code className="font-mono text-blue-600 dark:text-blue-400">{part.name}</code>
        {argSummary && <span className="text-gray-500 dark:text-gray-400 truncate">{argSummary}</span>}
        {isRunning && <Dots className="ml-auto" />}
      </summary>
      <div className="px-3 pb-3 pt-1 space-y-2 text-xs">
        <Section label="args">
          <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono">{prettyJSON(part.args)}</pre>
        </Section>
        {!isRunning && (
          <Section label="result">
            <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono max-h-80 overflow-y-auto">{prettyJSON(part.result)}</pre>
          </Section>
        )}
      </div>
    </details>
  )
}

function Section({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <div className="text-[10px] uppercase tracking-wide text-gray-400 dark:text-gray-500 mb-0.5">{label}</div>
      {children}
    </div>
  )
}

// Dots is the three-dot "working" indicator shared by tool calls and agent groups.
function Dots({ className = '' }: { className?: string }) {
  return (
    <span className={`flex items-center gap-1 text-gray-400 ${className}`}>
      <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.3s]" />
      <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.15s]" />
      <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce" />
    </span>
  )
}

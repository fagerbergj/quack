import { useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { ReactNode } from 'react'
import type { MessagePart, ToolCallPart, AgentPart, SelfRefinePart, JudgeVerdictPart } from './messageParts'
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
    case 'self_refine': return <SelfRefineBlock part={part} />
    case 'judge_verdict': return <JudgeBlock part={part} />
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

// AgentGroup renders one actor's container: a "→ agent" header plus its activity
// (thinking / tool calls / nested agents), windowed to the most recent few.
function AgentGroup({ part }: { part: AgentPart }) {
  const running = !part.done
  return (
    <details open className="my-2 rounded-lg border border-indigo-200 dark:border-indigo-900 bg-indigo-50/50 dark:bg-indigo-950/30 not-prose">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
        <span className="font-medium text-indigo-600 dark:text-indigo-400">→ {part.agent}</span>
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

// SelfRefineBlock is a one-line note that the worker self-refined its draft.
function SelfRefineBlock({ part }: { part: SelfRefinePart }) {
  return (
    <div className="my-1.5 px-1 text-xs text-gray-500 dark:text-gray-400 not-prose">
      ✎ self-refined {part.changed ? '— revised the draft' : '— no changes needed'}
    </div>
  )
}

// JudgeBlock renders one judge verdict: a pass/fail badge with the score, plus
// the feedback (shown when present, e.g. on a fail).
function JudgeBlock({ part }: { part: JudgeVerdictPart }) {
  const pct = Math.round(part.score * 100)
  const badge = part.passed
    ? 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400'
    : 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400'
  return (
    <div className="my-1.5 px-1 text-xs not-prose">
      <div className="flex items-center gap-2">
        <span className="text-gray-500 dark:text-gray-400">⚖ judge · round {part.round}</span>
        <span className={`rounded px-1.5 py-0.5 font-medium ${badge}`}>
          {part.passed ? 'pass' : 'revise'} · {pct}%
        </span>
      </div>
      {part.feedback && (
        <div className="mt-1 text-gray-500 dark:text-gray-400">{part.feedback}</div>
      )}
    </div>
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

import { useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { ReactNode } from 'react'
import type { Activity, ToolCall } from './messageParts'
import { summarizeArgs, prettyJSON } from './toolFormat'

// Re-export the data layer so import sites (`from '.../AgentParts'`) keep working;
// the run model + reducers live in messageParts.ts.
export * from './messageParts'

// RECENT is how many of a run's most recent activity items stay visible; older
// ones fold into a "⋯ N earlier" toggle so a long run stays scannable.
const RECENT = 3

// AssistantText renders model text as markdown.
export function AssistantText({ text }: { text: string }) {
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  )
}

// ActivityList renders a run's ordered activity (thinking + tool calls), windowed
// to the most recent few. Keys index into the append-only list so streaming
// reconciliation stays stable.
export function ActivityList({ activity }: { activity: Activity[] }) {
  const [showAll, setShowAll] = useState(false)
  const hidden = Math.max(0, activity.length - RECENT)
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
      {activity.slice(start).map((a, i) => (
        a.kind === 'thinking'
          ? <ThinkBlock key={start + i} text={a.text} />
          : <ToolBlock key={start + i} tool={a.tool} />
      ))}
    </>
  )
}

// ThinkBlock renders reasoning as a collapsed block.
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
export function ToolBlock({ tool }: { tool: ToolCall }) {
  const argSummary = summarizeArgs(tool.args)
  return (
    <details className="my-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 not-prose">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
        <code className="font-mono text-blue-600 dark:text-blue-400">{tool.name}</code>
        {argSummary && <span className="text-gray-500 dark:text-gray-400 truncate">{argSummary}</span>}
        {!tool.done && <Dots className="ml-auto" />}
      </summary>
      <div className="px-3 pb-3 pt-1 space-y-2 text-xs">
        <Section label="args">
          <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono">{prettyJSON(tool.args)}</pre>
        </Section>
        {tool.done && (
          <Section label="result">
            <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono max-h-80 overflow-y-auto">{prettyJSON(tool.result)}</pre>
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

// Dots is the three-dot "working" indicator shared by tool calls and run cards.
export function Dots({ className = '' }: { className?: string }) {
  return (
    <span className={`flex items-center gap-1 text-gray-400 ${className}`}>
      <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.3s]" />
      <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.15s]" />
      <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce" />
    </span>
  )
}

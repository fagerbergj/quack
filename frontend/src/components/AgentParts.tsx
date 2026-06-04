import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useState, useEffect, type ReactNode } from 'react'

export type MessagePart = TextPart | ThinkingPart | ToolCallPart | ConfirmationPart

export interface TextPart {
  kind: 'text'
  text: string
}

export interface ThinkingPart {
  kind: 'thinking'
  text: string
}

export interface ToolCallPart {
  kind: 'tool_call'
  name: string
  args: Record<string, unknown>
  result?: unknown
}

export interface ConfirmationPart {
  kind: 'confirmation'
  callId: string
  toolName: string
  hint: string
  field: string
  stage: string
  before: string
  after: string
  status: 'pending' | 'approved' | 'rejected'
}

export function AssistantParts({ parts, showCursor, onDecideConfirmation }: {
  parts: MessagePart[]
  showCursor?: boolean
  onDecideConfirmation?: (callId: string, confirmed: boolean, content?: string) => void
}) {
  const last = parts[parts.length - 1]
  const cursorAfterText = showCursor && last?.kind === 'text'
  return (
    <>
      {parts.map((p, i) => {
        if (p.kind === 'text') return <AssistantText key={i} text={p.text} />
        if (p.kind === 'thinking') return <ThinkBlock key={i} text={p.text} />
        if (p.kind === 'tool_call') return <ToolBlock key={i} part={p} />
        return <ConfirmationBlock key={i} part={p} onDecide={onDecideConfirmation} />
      })}
      {cursorAfterText && (
        <span className="inline-block w-1.5 h-4 bg-gray-400 animate-pulse ml-0.5 align-middle" />
      )}
    </>
  )
}

export function appendStreamingPart(parts: MessagePart[], kind: 'text' | 'thinking', text: string): MessagePart[] {
  const next = [...parts]
  const last = next[next.length - 1]
  if (last && last.kind === kind) {
    next[next.length - 1] = { ...last, text: last.text + text }
  } else {
    next.push({ kind, text })
  }
  return next
}

export const appendTextPart = (parts: MessagePart[], text: string) => appendStreamingPart(parts, 'text', text)
export const appendThinkingPart = (parts: MessagePart[], text: string) => appendStreamingPart(parts, 'thinking', text)

export function appendToolCall(parts: MessagePart[], name: string, args: Record<string, unknown>): MessagePart[] {
  return [...parts, { kind: 'tool_call', name, args }]
}

export function fillToolResult(parts: MessagePart[], name: string, result: unknown): MessagePart[] {
  const next = [...parts]
  for (let i = next.length - 1; i >= 0; i--) {
    const p = next[i]
    if (p.kind === 'tool_call' && p.name === name && p.result === undefined) {
      next[i] = { ...p, result }
      return next
    }
  }
  return next
}

export function appendConfirmation(parts: MessagePart[], part: Omit<ConfirmationPart, 'kind' | 'status'>): MessagePart[] {
  return [...parts, { kind: 'confirmation', status: 'pending', ...part }]
}

export function markConfirmation(parts: MessagePart[], callId: string, status: ConfirmationPart['status']): MessagePart[] {
  return parts.map(p => (p.kind === 'confirmation' && p.callId === callId ? { ...p, status } : p))
}

export function partsToText(parts: MessagePart[]): string {
  return parts.filter(p => p.kind === 'text').map(p => (p as TextPart).text).join('')
}

export function AssistantText({ text }: { text: string }) {
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  )
}

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

export function ToolBlock({ part }: { part: ToolCallPart }) {
  const argSummary = summarizeArgs(part.args)
  const isRunning = part.result === undefined
  return (
    <details className="my-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 not-prose">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs flex items-center gap-2">
        <code className="font-mono text-blue-600 dark:text-blue-400">{part.name}</code>
        {argSummary && <span className="text-gray-500 dark:text-gray-400 truncate">{argSummary}</span>}
        {isRunning && (
          <span className="ml-auto flex items-center gap-1 text-gray-400">
            <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.3s]" />
            <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.15s]" />
            <span className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce" />
          </span>
        )}
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

function summarizeArgs(args: Record<string, unknown>): string {
  for (const key of ['query', 'id']) {
    const v = args[key]
    if (typeof v === 'string' && v) return JSON.stringify(v)
  }
  return ''
}

function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

export function ConfirmationBlock({ part, onDecide }: {
  part: ConfirmationPart
  onDecide?: (callId: string, confirmed: boolean, content?: string) => void
}) {
  const [edited, setEdited] = useState(part.after)
  const [activeTab, setActiveTab] = useState<'diff' | 'editable'>('diff')
  const [collapsed, setCollapsed] = useState(false)
  const pending = part.status === 'pending'

  useEffect(() => {
    if (part.status !== 'pending') {
      setCollapsed(true)
    }
  }, [part.status])

  useEffect(() => {
    setActiveTab('diff')
    setEdited(part.after)
  }, [part.callId, part.after])

  const statusStyles =
    part.status === 'approved'
      ? {
          border: 'border-green-300 dark:border-green-700',
          bg: 'bg-green-50 dark:bg-green-950/30',
          text: 'text-green-800 dark:text-green-400',
          hint: 'text-green-700 dark:text-green-400',
          tabInactive: 'text-green-600 dark:text-green-500 hover:text-green-800 dark:hover:text-green-300',
        }
      : part.status === 'rejected'
      ? {
          border: 'border-red-300 dark:border-red-700',
          bg: 'bg-red-50 dark:bg-red-950/30',
          text: 'text-red-800 dark:text-red-400',
          hint: 'text-red-700 dark:text-red-400',
          tabInactive: 'text-red-600 dark:text-red-500 hover:text-red-800 dark:hover:text-red-300',
        }
      : {
          border: 'border-amber-200 dark:border-amber-800',
          bg: 'bg-amber-50 dark:bg-amber-950/30',
          text: 'text-amber-800 dark:text-amber-300',
          hint: 'text-amber-700 dark:text-amber-400',
          tabInactive: 'text-amber-600 dark:text-amber-500 hover:text-amber-800 dark:hover:text-amber-300',
        }

  return (
    <div className={`my-2 rounded-lg border ${statusStyles.border} ${statusStyles.bg} not-prose`}>
      {!collapsed && (
        <>
          <div className={`px-3 py-2 border-b ${statusStyles.border} flex items-center gap-2`}>
            <span className={`text-xs font-semibold uppercase tracking-wide ${statusStyles.text}`}>
              Approval needed
            </span>
            <span className={`text-xs ${statusStyles.hint}`}>{part.hint}</span>
            {part.status !== 'pending' && (
              <button
                onClick={() => setCollapsed(true)}
                className={`ml-auto text-xs font-medium ${statusStyles.text} hover:underline`}
              >
                {part.status} (collapse)
              </button>
            )}
          </div>

          <div className="p-3 space-y-3">
          <div className={`flex gap-2 border-b ${statusStyles.border}`}>
            <button
              onClick={() => setActiveTab('diff')}
              className={`px-3 py-1.5 text-xs font-medium transition-colors ${
                activeTab === 'diff'
                  ? `${statusStyles.text} border-b-2 border-current`
                  : statusStyles.tabInactive
              }`}
            >
              Diff
            </button>
            <button
              onClick={() => setActiveTab('editable')}
              className={`px-3 py-1.5 text-xs font-medium transition-colors ${
                activeTab === 'editable'
                  ? `${statusStyles.text} border-b-2 border-current`
                  : statusStyles.tabInactive
              }`}
            >
              Editable
            </button>
          </div>

          <div className="min-h-[200px]">
            {activeTab === 'diff' ? (
              <div className="text-sm text-gray-600 dark:text-gray-300">
                <p className="mb-2 font-medium">Before:</p>
                <p className="bg-gray-100 dark:bg-gray-800 p-3 rounded mb-2">{part.before}</p>
                <p className="font-medium">After:</p>
                <textarea
                  className="w-full rounded border border-gray-300 dark:border-gray-600 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100"
                  rows={6}
                  value={edited}
                  onChange={e => setEdited(e.target.value)}
                  disabled={!pending}
                />
              </div>
            ) : (
              <div className="text-sm text-gray-600 dark:text-gray-300">
                <p className="mb-2 font-medium">Editable version:</p>
                <textarea
                  className="w-full rounded border border-gray-300 dark:border-gray-600 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100"
                  rows={6}
                  value={edited}
                  onChange={e => setEdited(e.target.value)}
                  disabled={!pending}
                />
              </div>
            )}
          </div>

          {pending && (
            <div className="flex items-center gap-2">
              <button
                onClick={() => onDecide?.(part.callId, true, edited)}
                className="px-3 py-1.5 text-sm font-medium bg-green-600 text-white rounded-lg hover:bg-green-700"
              >
                Approve
              </button>
              <button
                onClick={() => onDecide?.(part.callId, false)}
                className="px-3 py-1.5 text-sm font-medium border border-gray-300 dark:border-gray-600 text-gray-700 dark:text-gray-200 rounded-lg hover:bg-gray-50 dark:hover:bg-gray-700"
              >
                Reject
              </button>
              {edited !== part.after && (
                <button
                  onClick={() => setEdited(part.after)}
                  className={`text-xs hover:underline ml-auto ${statusStyles.hint}`}
                >
                  Reset to model's proposal
                </button>
              )}
            </div>
          )}
          </div>
        </>
      )}

      {collapsed && (
        <button
          type="button"
          onClick={() => setCollapsed(false)}
          className={`w-full px-3 py-2 flex items-center gap-2 text-left ${statusStyles.bg} rounded-lg`}
        >
          <span className={`text-xs font-semibold uppercase tracking-wide ${statusStyles.text}`}>
            {part.status}
          </span>
          <span className="text-xs text-gray-500 dark:text-gray-400">{part.hint}</span>
          <span className={`ml-auto text-xs ${statusStyles.hint}`}>Expand</span>
        </button>
      )}
    </div>
  )
}

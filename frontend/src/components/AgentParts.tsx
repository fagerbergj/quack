import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { ReactNode } from 'react'

// MessagePart is one ordered chunk of an assistant message: model text,
// out-of-band reasoning (collapsible), or a tool call (collapsible).
// Adapted from document-pipeline; the confirmation part is out of scope for M0.
export type MessagePart = TextPart | ThinkingPart | ToolCallPart

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
  // result is set once the tool returns; while undefined we render a
  // "running…" indicator inside the collapsed block.
  result?: unknown
}

// AssistantParts renders a sequence of MessageParts in order: text via
// markdown, thinking + tool_call as collapsed-by-default blocks.
export function AssistantParts({ parts, showCursor }: {
  parts: MessagePart[]
  showCursor?: boolean
}) {
  const last = parts[parts.length - 1]
  const cursorAfterText = showCursor && last?.kind === 'text'
  return (
    <>
      {parts.map((p, i) => {
        if (p.kind === 'text') return <AssistantText key={i} text={p.text} />
        if (p.kind === 'thinking') return <ThinkBlock key={i} text={p.text} />
        return <ToolBlock key={i} part={p} />
      })}
      {cursorAfterText && (
        <span className="inline-block w-1.5 h-4 bg-gray-400 animate-pulse ml-0.5 align-middle" />
      )}
    </>
  )
}

// appendStreamingPart merges a streamed chunk into the last part if it has the
// same kind, otherwise opens a new part. Shared by text and thinking.
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

// appendToolCall pushes a new tool_call part with no result yet.
export function appendToolCall(parts: MessagePart[], name: string, args: Record<string, unknown>): MessagePart[] {
  return [...parts, { kind: 'tool_call', name, args }]
}

// fillToolResult attaches a result to the most recent matching tool_call that
// doesn't yet have one.
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

// partsToText concatenates the text parts (for copy/download). Thinking + tool
// payloads are omitted.
export function partsToText(parts: MessagePart[]): string {
  return parts.filter((p): p is TextPart => p.kind === 'text').map(p => p.text).join('')
}

// AssistantText renders model text as markdown.
export function AssistantText({ text }: { text: string }) {
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
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
  for (const key of ['query', 'id', 'q']) {
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

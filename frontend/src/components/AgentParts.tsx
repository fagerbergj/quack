import { useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeRaw from 'rehype-raw'
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize'
import rehypeHighlight from 'rehype-highlight'
import 'highlight.js/styles/github-dark.css'
import type { ComponentPropsWithoutRef } from 'react'
import type { Activity, ToolCall } from './messageParts'
import { agentLabel } from './messageParts'
import { summarizeArgs, previewLine, toolFailed } from './toolFormat'
import { Expandable } from './Expandable'
import { ToolCallView } from './ToolCallView'

// Answer text is Markdown that may embed a little raw HTML — notably the
// collapsible `<details><summary>Sources</summary>…</details>` block the
// researcher/synthesizer emit. react-markdown drops raw HTML by default, so we
// enable rehype-raw to parse it; because the text is model-authored (and shaped
// by fetched web content), we then run rehype-sanitize to strip anything unsafe,
// allowing only <details>/<summary> on top of the default-safe element set.
const mdSchema = {
  ...defaultSchema,
  tagNames: [...(defaultSchema.tagNames ?? []), 'details', 'summary'],
}

// Re-export the data layer so import sites (`from '.../AgentParts'`) keep working;
// the run model + reducers live in messageParts.ts.
export * from './messageParts'

// RECENT is how many of a run's most recent activity items stay visible; older
// ones fold into a "⋯ N earlier" toggle so a long run stays scannable.
const RECENT = 3

// CopyablePre wraps a fenced code block in a relative container with a one-click
// copy button. rehype-highlight runs AFTER sanitize (see AssistantText), so the
// hljs token markup is intact by the time this renders.
function CopyablePre({ children, ...props }: ComponentPropsWithoutRef<'pre'>) {
  const ref = useRef<HTMLPreElement>(null)
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard.writeText(ref.current?.textContent ?? '')
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <div className="relative group not-prose">
      <button
        type="button"
        onClick={copy}
        aria-label="Copy code"
        className="absolute right-2 top-2 z-10 rounded border border-gray-600 bg-gray-800/80 px-2 py-0.5 text-[11px] text-gray-300 opacity-0 group-hover:opacity-100 focus:opacity-100 transition-opacity hover:bg-gray-700"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
      <pre ref={ref} {...props}>{children}</pre>
    </div>
  )
}

// AssistantText renders model text as markdown. rehype-highlight is placed LAST
// so its hljs classes/spans aren't stripped by rehype-sanitize (the code's
// `language-*` class survives sanitize, so highlight still detects the language).
export function AssistantText({ text }: { text: string }) {
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlight]}
        components={{ pre: CopyablePre }}
      >{text}</ReactMarkdown>
    </div>
  )
}

// BubbleHeader is the compact author line atop an assistant bubble: who produced
// it, what model, and how many tokens it cost. Shared by the answer bubble (a DAG
// turn's terminal node, or the orchestrator's own plain reply) — real usage only,
// no estimate: model/tokens are simply omitted when not (yet) known.
export function BubbleHeader({ agent, model, tokens }: { agent: string; model?: string; tokens?: number }) {
  return (
    <div className="flex items-center gap-2 mb-2 text-[10px] text-gray-400 dark:text-gray-500">
      <span className="font-semibold text-gray-500 dark:text-gray-400">{agentLabel(agent)}</span>
      {model && (
        <span className="font-mono truncate max-w-[160px]" title={model}>{model}</span>
      )}
      {tokens != null && tokens > 0 && (
        <span className="tabular-nums">{tokens.toLocaleString()} tok</span>
      )}
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

// ThinkBlock renders reasoning as a single-line, collapsed-by-default summary
// (an icon + "Thought" + a truncated preview of the text) that expands to the
// full chain-of-thought — Open WebUI's cleaner ethos (#385): scannable at a
// glance, full detail on demand rather than a standing wall of text. Long
// reasoning is height-locked (Expandable) once expanded so it still can't wall
// off the node. A thin left rail (not a boxed card) keeps it visually
// subordinate to the tool calls it sits beside.
function ThinkBlock({ text }: { text: string }) {
  return (
    <details className="group my-0.5 not-prose">
      <summary className="cursor-pointer select-none flex items-center gap-1.5 py-0.5 text-[11px] text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300">
        <span aria-hidden className="shrink-0">💭</span>
        <span className="italic shrink-0">Thought</span>
        <span className="truncate text-gray-300 dark:text-gray-600 group-open:hidden">{previewLine(text)}</span>
      </summary>
      <div className="ml-[7px] pl-2.5 pr-2 py-1 border-l border-gray-200 dark:border-gray-700 text-xs text-gray-500 dark:text-gray-400">
        <Expandable maxHeight={200} fade="from-white dark:from-gray-800">
          <div className="whitespace-pre-wrap font-mono">{text}</div>
        </Expandable>
      </div>
    </details>
  )
}

// ToolBlock renders a tool call as a single-line, collapsed-by-default summary
// — a status icon, the tool name, and a truncated representative arg — that
// expands to a per-tool rich view (ToolCallView): a diff for edit_file, a
// formatted view for the other common tools, a tidy fallback otherwise.
// Refined toward the same compact, low-noise ethos as ThinkBlock (#385): a
// thin left rail on expand instead of a bordered card, and a check/cross
// status icon (done vs failed) rather than the "working" dots once settled.
export function ToolBlock({ tool }: { tool: ToolCall }) {
  const argSummary = summarizeArgs(tool.args)
  return (
    <details className="group my-0.5 not-prose">
      <summary className="cursor-pointer select-none flex items-center gap-1.5 py-0.5 text-[11px]">
        <ToolStatusIcon tool={tool} />
        <code className="font-mono text-gray-600 dark:text-gray-300 shrink-0">{tool.name}</code>
        {argSummary && <span className="text-gray-400 dark:text-gray-500 truncate">{argSummary}</span>}
      </summary>
      <div className="ml-[7px] pl-2.5 pr-2 py-1 border-l border-gray-200 dark:border-gray-700 text-xs">
        <ToolCallView tool={tool} />
      </div>
    </details>
  )
}

// ToolStatusIcon is the compact status marker heading a tool-call summary
// line: the "working" dots while in flight, a check once it completed, a
// cross when its result carried an error — status conveyed by icon+colour
// together (WCAG 1.4.1), not colour alone.
function ToolStatusIcon({ tool }: { tool: ToolCall }) {
  if (!tool.done) return <Dots size="w-1 h-1" />
  return toolFailed(tool.result)
    ? <span className="text-red-500 dark:text-red-400 shrink-0" aria-hidden>✗</span>
    : <span className="text-green-600 dark:text-green-400 shrink-0" aria-hidden>✓</span>
}

// Dots is the three-dot "working" indicator shared by tool calls, run cards, and
// the live/pending answer bubbles. `size` is a Tailwind w/h class pair.
export function Dots({ className = '', size = 'w-1.5 h-1.5' }: { className?: string; size?: string }) {
  return (
    <span className={`flex items-center gap-1 text-gray-400 ${className}`}>
      <span className={`${size} rounded-full bg-gray-400 animate-bounce [animation-delay:-0.3s]`} />
      <span className={`${size} rounded-full bg-gray-400 animate-bounce [animation-delay:-0.15s]`} />
      <span className={`${size} rounded-full bg-gray-400 animate-bounce`} />
    </span>
  )
}

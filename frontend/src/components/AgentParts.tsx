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
import { summarizeArgs, previewLine, toolFailed, copyPayload } from './toolFormat'
import { Expandable } from './Expandable'
import { ToolCallView } from './ToolCallView'
import { CopyButton } from './CopyButton'
import { StatusDot, type DotStatus } from './StatusDot'

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

// ACP_AGENTS mirrors config/quack.yaml's acp-bound bundles (code-implementer,
// code-reviewer, code-explorer) — the only agents whose tool calls arrive over
// the Agent Client Protocol, remapped by internal/acp/translate.go rather than
// invoked as a native quack tool. There's no per-call wire marker (#404) — the
// agent name is already threaded onto every run and ACP/native bundles never
// overlap, so it's a clean, no-backend-change signal for the "ACP" badge.
const ACP_AGENTS = new Set(['code-implementer', 'code-reviewer', 'code-explorer'])

export function isAcpAgent(agent?: string): boolean {
  return !!agent && ACP_AGENTS.has(agent)
}

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
// no estimate: model/tokens are simply omitted when not (yet) known. `status`
// is optional (#416): the live orchestrator card passes it to show a StatusDot
// to the left of the name, matching DagNode's header — omitted for completed
// turns and DAG-terminal-node attribution, which have no live status to show.
export function BubbleHeader({ agent, model, tokens, status }: { agent: string; model?: string; tokens?: number; status?: DotStatus }) {
  return (
    <div className="flex items-center gap-2 mb-2 text-[10px] text-gray-400 dark:text-gray-500">
      {status && <StatusDot status={status} />}
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

// ThoughtIcon is a crisp inline SVG (currentColor, so it inherits the
// muted text colour and stays legible in both themes) — replaces an earlier
// emoji glyph that rendered pixelated/mismatched-colour on a dark background,
// since emoji are drawn from the platform's colour-emoji font rather than the
// surrounding text style.
function ThoughtIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" className="shrink-0 w-3 h-3" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round">
      <path d="M8 1.75a5.25 5.25 0 0 0-4.42 8.08L3 13.25l3.6-1.13A5.25 5.25 0 1 0 8 1.75Z" />
      <circle cx="5.4" cy="7" r="0.6" fill="currentColor" stroke="none" />
      <circle cx="8" cy="7" r="0.6" fill="currentColor" stroke="none" />
      <circle cx="10.6" cy="7" r="0.6" fill="currentColor" stroke="none" />
    </svg>
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
        <ThoughtIcon />
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

// AcpBadge marks a tool call that arrived over the Agent Client Protocol
// (an external code-implementer/-reviewer/-explorer subprocess, #404): its
// payload shapes aren't fully ours to control, so ToolCallView renders it
// best-effort — the badge sets that expectation, and the copy button (always
// present) is the escape hatch for whatever doesn't render nicely.
export function AcpBadge() {
  return (
    <span
      title="Run by an external ACP agent — rendered best-effort"
      className="shrink-0 text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded bg-indigo-50 text-indigo-600 dark:bg-indigo-900/30 dark:text-indigo-300"
    >
      ACP
    </span>
  )
}

// ToolBlock renders a tool call as a single-line, collapsed-by-default summary
// — a status icon, the tool name, and a truncated representative arg — that
// expands to a per-tool rich view (ToolCallView): a diff for edit_file, a
// formatted view for the other common tools, a tidy fallback otherwise.
// Refined toward the same compact, low-noise ethos as ThinkBlock (#385): a
// thin left rail on expand instead of a bordered card, and a check/cross
// status icon (done vs failed) rather than the "working" dots once settled.
  // The copy button sits in a sibling header row layered over the summary's
  // right edge (#435), not nested inside the `<summary>` itself: a `<summary>`
// is already the disclosure's own interactive control, and a button nested
// inside it is invalid HTML that breaks keyboard use (Enter/Space on the
// summary vs. the nested button conflict).
export function ToolBlock({ tool }: { tool: ToolCall }) {
  const argSummary = summarizeArgs(tool.args)
  return (
    <div className="relative my-0.5 not-prose">
      <details className="group">
        <summary className="cursor-pointer select-none flex items-center gap-1.5 py-0.5 pr-6 text-[11px]">
          <ToolStatusIcon tool={tool} />
         <code className="font-mono text-gray-600 dark:text-gray-300 shrink-0">{tool.name}</code>
           {argSummary && <span className="text-gray-400 dark:text-gray-500 truncate">{argSummary}</span>}
        </summary>
        <div className="ml-[7px] pl-2.5 pr-2 py-1 border-l border-gray-200 dark:border-gray-700 text-xs">
          <ToolCallView tool={tool} />
        </div>
      </details>
      <span className="absolute right-0 top-0.5 shrink-0">
        <CopyButton text={copyPayload(tool.args, tool.result, tool.done)} label="Copy tool call JSON" />
      </span>
    </div>
  )
}

// ToolStatusIcon is the compact status marker heading a tool-call summary
// line: the "working" dots while in flight, a check once it completed, a
// cross when its result carried an error — status conveyed by icon+colour
// together (WCAG 1.4.1), not colour alone.
function ToolStatusIcon({ tool }: { tool: ToolCall }) {
  if (!tool.done) return <Dots variant="compact" size="w-1 h-1" />
  return toolFailed(tool.result)
    ? <span className="text-red-500 dark:text-red-400 shrink-0" aria-hidden>✗</span>
    : <span className="text-green-600 dark:text-green-400 shrink-0" aria-hidden>✓</span>
}

// Dots is the "working" indicator. Two variants (#421): the chat-level answer
// bubble's loading state ('chat', the default) keeps the three staggered
// `animate-bounce` dots, gray to match the rest of the UI's muted status
// chrome — count is deliberate there, confirmed elsewhere (#424) not to
// shrink to one. The 'compact' variant (a single blue `animate-pulse` dot)
// is for tight, high-multiplicity spots — a tool call's status icon, where a
// chat can have several of these mounted at once and three independent
// bounce timelines apiece added up to real animation cost without the
// horizontal room three dots need anyway. `size` is a Tailwind w/h class pair.
export function Dots({ className = '', size = 'w-1.5 h-1.5', variant = 'chat' }: { className?: string; size?: string; variant?: 'chat' | 'compact' }) {
  if (variant === 'compact') {
    return (
      <span className={`inline-flex items-center ${className}`} aria-label="working">
        <span className={`inline-block ${size} rounded-full bg-blue-500 animate-pulse`} />
      </span>
    )
  }
  return (
    <span className={`inline-flex items-center gap-0.5 ${className}`} aria-label="working">
      <span className={`inline-block ${size} rounded-full bg-gray-400 dark:bg-gray-500 animate-bounce [animation-delay:-0.3s]`} />
      <span className={`inline-block ${size} rounded-full bg-gray-400 dark:bg-gray-500 animate-bounce [animation-delay:-0.15s]`} />
      <span className={`inline-block ${size} rounded-full bg-gray-400 dark:bg-gray-500 animate-bounce`} />
    </span>
  )
}

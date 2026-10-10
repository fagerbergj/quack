import { memo, useMemo, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeRaw from 'rehype-raw'
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize'
import rehypeHighlightSubset from '../lib/rehypeHighlightSubset'
import 'highlight.js/styles/github-dark.css'
import type { ComponentPropsWithoutRef } from 'react'
import type { Element } from 'hast'
import type { Activity, ToolCall } from './messageParts'
import { Icon } from './Icon'
import { agentLabel, liveStatusLine } from './messageParts'
import { previewLine, toolFailed, toolActionLine, fmtTokenCount } from './toolFormat'
import { escapeUnmatchedBackticks } from '../lib/backticks'
import { Expandable } from './Expandable'
import { ToolCallView } from './ToolCallView'
import { CopyablePre } from './CopyablePre'
import { MermaidDiagram } from './MermaidDiagram'
import { isTrailingMermaidFenceOpen, lastSafeSplitOffset } from './mermaidSource'
import { StatusDot, type DotStatus } from './StatusDot'

// Answers carry raw `<details><summary>Sources</summary>` HTML; text is model-authored and web-shaped,
// so sanitize allows only <details>/<summary> beyond the default schema.
export const mdSchema = {
  ...defaultSchema,
  tagNames: [...(defaultSchema.tagNames ?? []), 'details', 'summary'],
}

export * from './messageParts'

// Older activity folds into a "⋯ N earlier" toggle so a long run stays scannable.
const RECENT = 3

// Mirrors config/quack.yaml's acp-bound bundles. There is no per-call wire marker, so the agent name
// decides; ACP and native bundles never overlap.
const ACP_AGENTS = new Set(['code-implementer', 'code-reviewer', 'code-explorer'])

export function isAcpAgent(agent?: string): boolean {
  return !!agent && ACP_AGENTS.has(agent)
}

// Reads the info string off the AST so it works even though highlight.js doesn't know mermaid.
function mermaidLang(preNode: Element | undefined): boolean {
  const code = preNode?.children.find((c): c is Element => c.type === 'element' && c.tagName === 'code')
  const classNames = code?.properties?.className
  return Array.isArray(classNames) && classNames.some(c => typeof c === 'string' && c.toLowerCase() === 'language-mermaid')
}

// Raw mermaid source, unaffected by highlighting markup applied to its <code>.
function hastText(node: Element | undefined): string {
  if (!node) return ''
  return node.children.map(c => (c.type === 'text' ? c.value : hastText(c as Element))).join('')
}

// A mermaid block renders as a diagram only once closed; only the last block can be open, so its end offset
// is the stable open-test. rehypeHighlightSubset runs after sanitize so its hljs classes survive.
function AssistantDocument({ text }: { text: string }) {
  const trailingOpen = useMemo(() => isTrailingMermaidFenceOpen(text), [text])
  const docEnd = useMemo(() => text.replace(/\s+$/, '').length, [text])
  const components = useMemo(() => ({
    pre: (props: ComponentPropsWithoutRef<'pre'> & { node?: Element }) => {
      const { node, children, ...rest } = props
      const isLastBlock = node?.position?.end?.offset === docEnd
      if (mermaidLang(node) && !(isLastBlock && trailingOpen)) {
        const codeNode = node?.children.find((c): c is Element => c.type === 'element' && c.tagName === 'code')
        return <MermaidDiagram code={hastText(codeNode)} />
      }
      return <CopyablePre {...rest}>{children}</CopyablePre>
    },
    // A wide table scrolls in its own box instead of pushing the bubble past the viewport.
    table: (props: ComponentPropsWithoutRef<'table'> & { node?: Element }) => {
      // eslint-disable-next-line @typescript-eslint/no-unused-vars
      const { node, ...rest } = props
      return <div className="overflow-x-auto"><table {...rest} /></div>
    },
  }), [trailingOpen, docEnd])
  // ReactMarkdown re-parses on every render regardless of prop equality, and a streaming parent
  // re-renders far more often than `text` changes.
  return useMemo(() => (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlightSubset]}
      components={components}
    >{text}</ReactMarkdown>
  ), [text, components])
}

// Settled prefix of the streaming split: same string keeps memo true, so it renders once per prefix advance.
const FrozenAssistantDocument = memo(AssistantDocument)

// Measured: 17k frozen + 2k live cut re-render from 65.6 to 5.7 ms/token.
const LIVE_TAIL_CHARS = 2000

// While streaming, splits at the last safe block boundary so only the tail re-parses per token. Once done,
// renders as ONE document so tables, footnotes and link refs spanning the split resolve.
export function AssistantText({ text, streaming = false }: { text: string; streaming?: boolean }) {
  // A stray backtick defeats CommonMark's backtick pairing for every later inline span.
  // Offsets below derive from this fixed string, since escaping shifts everything after it.
  const fixed = useMemo(() => escapeUnmatchedBackticks(text), [text])
  // Re-freezing re-parses the whole prefix, so advance only forward and in ~LIVE_TAIL_CHARS steps
  // to keep total streaming cost near-linear.
  const frozenCutRef = useRef(0)
  if (!streaming) {
    frozenCutRef.current = 0
  } else if (fixed.length - frozenCutRef.current > LIVE_TAIL_CHARS) {
    const next = lastSafeSplitOffset(fixed, fixed.length - LIVE_TAIL_CHARS)
    if (next > frozenCutRef.current) frozenCutRef.current = next
  }
  const cut = streaming ? frozenCutRef.current : 0
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none break-words">
      {cut > 0 ? (
        <>
          <FrozenAssistantDocument text={fixed.slice(0, cut)} />
          <AssistantDocument text={fixed.slice(cut)} />
        </>
      ) : (
        <AssistantDocument text={fixed} />
      )}
    </div>
  )
}

// A stopped node's draft shows "not reviewed"; with no draft the card is a bare "Stopped" marker.
export type StoppedBadge = 'draft' | 'bare'

export function stoppedBadge(stopped: boolean | undefined, text: string | undefined): StoppedBadge | undefined {
  if (!stopped) return undefined
  return text ? 'draft' : 'bare'
}

// Real usage only, no estimates: model/tokens are omitted until known. Only the live orchestrator card
// passes `status`; completed turns have no live status.
export function BubbleHeader({ agent, model, tokens, status, stopped }: { agent: string; model?: string; tokens?: number; status?: DotStatus; stopped?: StoppedBadge }) {
  return (
    <div className="flex items-center gap-2 mb-2 text-[11px] text-gray-500 dark:text-gray-400">
      {status && <StatusDot status={status} />}
      <span className="font-semibold text-gray-500 dark:text-gray-400">{agentLabel(agent)}</span>
      {stopped && (
        <span
          className="inline-flex items-center gap-0.5 font-medium text-amber-600 dark:text-amber-400"
          title="This agent was stopped before it finished, so this draft never passed the quality check"
        >
          <Icon name="warning" className="w-3 h-3" /> {stopped === 'draft' ? 'Stopped - not reviewed' : 'Stopped'}
        </span>
      )}
      {model && (
        <span className="font-mono truncate max-w-[160px]" title={model}>{model}</span>
      )}
      {tokens != null && tokens > 0 && (
        <span className="tabular-nums">{tokens.toLocaleString()} tokens</span>
      )}
    </div>
  )
}

// Keys index into the append-only list so streaming reconciliation stays stable.
export function ActivityList({ activity }: { activity: Activity[] }) {
  const [showAll, setShowAll] = useState(false)
  const hidden = Math.max(0, activity.length - RECENT)
  const start = showAll ? 0 : hidden
  return (
    <>
      {hidden > 0 && (
        <button
          onClick={() => setShowAll(s => !s)}
          className="min-h-[44px] -my-2 inline-flex items-center text-[11px] text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
        >
          {showAll ? '▾ show less' : `⋯ ${hidden} earlier`}
        </button>
      )}
      {activity.slice(start).map((a, i) => {
        switch (a.kind) {
          case 'thinking': return <ThinkBlock key={start + i} text={a.text} />
          case 'compaction': return <CompactionBlock key={start + i} {...a} />
          default: return <ToolBlock key={start + i} tool={a.tool} />
        }
      })}
    </>
  )
}

// Stands in for ActivityList while running: a full list re-renders on every SSE event and locks the tab
// on a busy node. One line, truncating rather than stacking on a phone.
export function LiveStatusLine({ activity }: { activity: Activity[] }) {
  const { thinking, tool, compacted } = liveStatusLine(activity)
  if (!thinking && !tool && !compacted) return null
  return (
    <div className="flex items-center gap-1.5 min-w-0 py-0.5 text-[11px] text-gray-500 dark:text-gray-400 not-prose">
      <Dots variant="compact" size="w-1 h-1" />
      {thinking && <span className="italic shrink-0">thinking</span>}
      {compacted && <span className="italic shrink-0">compacted</span>}
      {tool && <span className="truncate font-mono">{toolActionLine(tool.name, tool.args)}</span>}
    </div>
  )
}

// SVG, not an emoji: colour-emoji fonts render pixelated on dark backgrounds; currentColor follows the theme.
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

// Expanded reasoning is height-locked so it can't wall off the node; a thin rail, not a card,
// keeps it subordinate to tool calls.
function ThinkBlock({ text }: { text: string }) {
  return (
    <details className="group my-0.5 not-prose">
      <summary className="cursor-pointer select-none flex items-center gap-1.5 py-1 text-[11px] text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
        <ThoughtIcon />
        <span className="italic shrink-0">Thought</span>
        <span className="truncate text-gray-500 dark:text-gray-400 group-open:hidden">{previewLine(text)}</span>
      </summary>
      <div className="ml-[7px] pl-2.5 pr-2 py-1 border-l border-gray-200 dark:border-gray-700 text-xs text-gray-500 dark:text-gray-400">
        <Expandable maxHeight={200} fade="from-white dark:from-gray-800">
          <div className="whitespace-pre-wrap font-mono">{text}</div>
        </Expandable>
      </div>
    </details>
  )
}

// Marks a mid-round history rewrite. adk reports no before/after context size, so this shows the
// summarizer's own spend when known.
function CompactionBlock({ summaryInputTokens, summaryOutputTokens }: { summaryInputTokens?: number; summaryOutputTokens?: number }) {
  const hasTokens = !!summaryInputTokens || !!summaryOutputTokens
  return (
    <div
      className="flex items-center gap-1.5 my-0.5 py-0.5 text-[11px] text-gray-500 dark:text-gray-400 not-prose"
      aria-label={hasTokens ? `Context compacted, summarizer spent ${summaryInputTokens ?? 0} in / ${summaryOutputTokens ?? 0} out tokens` : 'Context compacted'}
    >
      <span aria-hidden>↯</span>
      <span className="italic">compacted</span>
      {hasTokens && (
        <span className="tabular-nums">{fmtTokenCount(summaryInputTokens ?? 0)} → {fmtTokenCount(summaryOutputTokens ?? 0)}</span>
      )}
    </div>
  )
}

// ACP payload shapes aren't ours to control, so ToolCallView renders them best-effort and the
// copy button is the escape hatch.
export function AcpBadge() {
  return (
    <span
      title="Run by an external ACP agent - rendered best-effort"
      className="shrink-0 text-[11px] font-semibold tracking-wide px-1 py-0.5 rounded bg-indigo-50 text-indigo-600 dark:bg-indigo-900/30 dark:text-indigo-300"
    >
      ACP
    </span>
  )
}

// Never nest a button inside <summary>: it is invalid HTML and breaks Enter/Space keyboard use.
export function ToolBlock({ tool }: { tool: ToolCall }) {
  const label = tool.name
  // The row shows "<verb> <target>"; the raw tool id lives in the tooltip.
  return (
    <div className="relative my-0.5 not-prose">
      <details className="group">
        <summary className="cursor-pointer select-none flex items-center gap-1.5 py-1 text-[11px]">
          <ToolStatusIcon tool={tool} />
          <span className="text-gray-600 dark:text-gray-300 truncate" title={label}>{toolActionLine(label, tool.args)}</span>
        </summary>
        <div className="ml-[7px] pl-2.5 pr-2 py-1 border-l border-gray-200 dark:border-gray-700 text-xs">
          <ToolCallView tool={tool} />
        </div>
      </details>
    </div>
  )
}

// Icon plus colour, never colour alone (WCAG 1.4.1).
function ToolStatusIcon({ tool }: { tool: ToolCall }) {
  if (!tool.done) return <Dots variant="compact" size="w-1 h-1" />
  return toolFailed(tool.result)
    ? <Icon name="close" className="w-3 h-3 text-red-500 dark:text-red-400 shrink-0" />
    : <Icon name="check" className="w-3 h-3 text-green-600 dark:text-green-400 shrink-0" />
}

// 'chat' keeps three bounce dots on purpose; 'compact' is one pulse dot for high-multiplicity spots where
// three bounce timelines cost real animation time. `size` is a Tailwind w/h class pair.
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

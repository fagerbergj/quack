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

// Answer text is Markdown with a little raw HTML - the collapsible
// `<details><summary>Sources</summary>` block the researcher/synthesizer emit.
// rehype-raw parses it; rehype-sanitize (model-authored, web-shaped text) strips everything but <details>/<summary>.
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

// Mirrors config/quack.yaml's acp-bound bundles - the only agents whose tool
// calls arrive over ACP, remapped by internal/acp/translate.go. No per-call
// wire marker (#404); the agent name is threaded onto every run and ACP/native bundles never overlap.
const ACP_AGENTS = new Set(['code-implementer', 'code-reviewer', 'code-explorer'])

export function isAcpAgent(agent?: string): boolean {
  return !!agent && ACP_AGENTS.has(agent)
}

// Reads the <code class="language-mermaid"> info string straight off the AST,
// independent of how rehype-highlight renders children - robust for a language
// highlight.js doesn't know.
function mermaidLang(preNode: Element | undefined): boolean {
  const code = preNode?.children.find((c): c is Element => c.type === 'element' && c.tagName === 'code')
  const classNames = code?.properties?.className
  return Array.isArray(classNames) && classNames.some(c => typeof c === 'string' && c.toLowerCase() === 'language-mermaid')
}

// hastText concatenates a hast node's text descendants - the raw mermaid
// source, unaffected by any syntax-highlighting markup applied to its <code>.
function hastText(node: Element | undefined): string {
  if (!node) return ''
  return node.children.map(c => (c.type === 'text' ? c.value : hastText(c as Element))).join('')
}

// One markdown parse of `text` (audit finding 2: re-parsing the whole doc
// past ~20k chars cost 65+ ms/token). A ```mermaid block renders as a diagram only
// once its closing fence arrives - an unterminated fence swallows the doc tail, so at most one can be open (the last block); node.position.end.offset vs isTrailingMermaidFenceOpen(text) is the stable open-test. rehypeHighlightSubset runs LAST, so sanitize keeps its hljs classes.
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
    // A wide table scrolls inside its own box instead of pushing the bubble
    // past the 70ch measure (or the viewport on a phone).
    table: (props: ComponentPropsWithoutRef<'table'> & { node?: Element }) => {
      // eslint-disable-next-line @typescript-eslint/no-unused-vars
      const { node, ...rest } = props
      return <div className="overflow-x-auto"><table {...rest} /></div>
    },
  }), [trailingOpen, docEnd])
  // Memoize the parsed output itself: ReactMarkdown re-parses on every call
  // regardless of prop equality, and the parent (a streaming bubble) re-renders
  // far more often than `text`/`components` actually change.
  return useMemo(() => (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlightSubset]}
      components={components}
    >{text}</ReactMarkdown>
  ), [text, components])
}

// The settled-prefix half of the streaming split: identical string content
// keeps memo's default shallow comparison true, so it renders exactly once per
// prefix advance rather than once per token.
const FrozenAssistantDocument = memo(AssistantDocument)

// How much of the tail stays live/unmemoized. Measured (src/perf/split.bperf.test.tsx,
// audit finding 2): 17k frozen + 2k live cut whole-document re-render from
// 65.6 to 5.7 ms/token at 19k chars - most of a long answer no longer re-parses per token.
const LIVE_TAIL_CHARS = 2000

// Renders model text as markdown. While `streaming`, splits at the last safe
// CommonMark block boundary (a blank line outside any open fence - lastSafeSplitOffset)
// before the live tail: the prefix renders through the memoized FrozenAssistantDocument, only the short tail re-parses per token. Once the stream ends, the whole text renders through ONE AssistantDocument - a table, footnote, or link reference spanning the old split must resolve as a single document, not two halves.
export function AssistantText({ text, streaming = false }: { text: string; streaming?: boolean }) {
  // #746 item 16: a stray punctuation backtick earlier in the text defeats
  // CommonMark's greedy backtick pairing for every later inline code span -
  // fix it before parsing, never inside a fenced block. Offsets below derive from THIS fixed string (escaping shifts everything after it).
  const fixed = useMemo(() => escapeUnmatchedBackticks(text), [text])
  // The frozen boundary only advances forward, and only once the live tail
  // would exceed LIVE_TAIL_CHARS: re-freezing re-parses the WHOLE prefix
  // (O(prefix)), so ~2000-char steps keep total streaming cost near-linear, not quadratic.
  const frozenCutRef = useRef(0)
  if (!streaming) {
    frozenCutRef.current = 0
  } else if (fixed.length - frozenCutRef.current > LIVE_TAIL_CHARS) {
    const next = lastSafeSplitOffset(fixed, fixed.length - LIVE_TAIL_CHARS)
    if (next > frozenCutRef.current) frozenCutRef.current = next
  }
  const cut = streaming ? frozenCutRef.current : 0
  return (
    <div className="prose prose-sm dark:prose-invert max-w-[70ch] break-words">
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

// Compact author line atop an assistant bubble: real usage only, no
// estimate - model/tokens omitted when not (yet) known. `status` is optional (#416):
// only the live orchestrator card shows a StatusDot by the name (matching DagNode's header) - completed turns and DAG-terminal attribution have no live status.
export function BubbleHeader({ agent, model, tokens, status }: { agent: string; model?: string; tokens?: number; status?: DotStatus }) {
  return (
    <div className="flex items-center gap-2 mb-2 text-[11px] text-gray-500 dark:text-gray-400">
      {status && <StatusDot status={status} />}
      <span className="font-semibold text-gray-500 dark:text-gray-400">{agentLabel(agent)}</span>
      {model && (
        <span className="font-mono truncate max-w-[160px]" title={model}>{model}</span>
      )}
      {tokens != null && tokens > 0 && (
        <span className="tabular-nums">{tokens.toLocaleString()} tokens</span>
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

// The RUNNING substitute for ActivityList (#725): a full activity list
// re-renders markdown/Expandable/ToolCallView on every streamed SSE event,
// which locks the tab on a busy node; this renders two short lines (current thinking + latest tool call) no matter how long the run.
export function LiveStatusLine({ activity }: { activity: Activity[] }) {
  const { thinking, tool, compacted } = liveStatusLine(activity)
  if (!thinking && !tool && !compacted) return null
  // One line, not one per fact: on a phone the card's running state is this
  // line plus the header, so the tool action truncates rather than stacking.
  return (
    <div className="flex items-center gap-1.5 min-w-0 py-0.5 text-[11px] text-gray-500 dark:text-gray-400 not-prose">
      <Dots variant="compact" size="w-1 h-1" />
      {thinking && <span className="italic shrink-0">thinking</span>}
      {compacted && <span className="italic shrink-0">compacted</span>}
      {tool && <span className="truncate font-mono">{toolActionLine(tool.name, tool.args)}</span>}
    </div>
  )
}

// Inline SVG with currentColor so it inherits the muted text colour in both
// themes - replaces an earlier emoji glyph, which the platform colour-emoji
// font rendered pixelated/mismatched on a dark background.
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

// Reasoning as a single-line, collapsed-by-default summary that expands to the
// full chain-of-thought on demand (#385). Long reasoning is height-locked
// (Expandable) once expanded so it can't wall off the node; a thin left rail (not a boxed card) keeps it visually subordinate to tool calls.
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

// One-line inline row marking a mid-round history rewrite; never expands.
// adk reports no before/after conversation-size total (unlike the pre-#1239
// quack engine), so this shows the summarizer's own spend when known and falls back to a bare label otherwise.
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

// Marks a tool call that arrived over the Agent Client Protocol (an external
// code-implementer/-reviewer/-explorer subprocess, #404): its payload shapes
// aren't fully ours to control, so ToolCallView renders it best-effort and the always-present copy button is the escape hatch.
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

// A single-line, collapsed-by-default tool-call summary expanding to a per-tool
// rich view (ToolCallView: a diff for edit_file, formatted views otherwise);
// #385 styling: thin left rail on expand, check/cross status icon instead of "working" dots once settled. The copy button sits in a sibling header row, not nested inside the <summary> (#435): a button inside a summary is invalid HTML that breaks keyboard use (Enter/Space conflict).
export function ToolBlock({ tool }: { tool: ToolCall }) {
  // #1312 made the relay carry the real MCP tool name instead of "other",
  // so name alone is always meaningful now.
  const label = tool.name
  // The human-readable "<verb> <target>" is the row; the raw tool id is
  // demoted to the tooltip (audit #14).
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

// Compact status marker heading a tool-call summary line: "working" dots in
// flight, a check once it completed, a cross on error - status conveyed by
// icon+colour together (WCAG 1.4.1), not colour alone.
function ToolStatusIcon({ tool }: { tool: ToolCall }) {
  if (!tool.done) return <Dots variant="compact" size="w-1 h-1" />
  return toolFailed(tool.result)
    ? <Icon name="close" className="w-3 h-3 text-red-500 dark:text-red-400 shrink-0" />
    : <Icon name="check" className="w-3 h-3 text-green-600 dark:text-green-400 shrink-0" />
}

// The "working" indicator. Two variants (#421): 'chat' = three staggered
// bounce dots - the count is deliberate, do not shrink to one (#424); 'compact'
// = a single pulse dot for high-multiplicity spots where three independent bounce timelines are real animation cost. `size` is a Tailwind w/h class pair.
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

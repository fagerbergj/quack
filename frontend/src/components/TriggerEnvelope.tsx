import { useMemo } from 'react'
import type { ReactNode } from 'react'
import { AssistantText } from './AgentParts'
import { Expandable } from './Expandable'
import {
  parseEnvelope,
  commentsSummaryLabel,
  changedFilesSummaryLabel,
  accumulateComments,
  type EnvelopeBlock,
} from './envelope'

// Re-export the parser so import sites that only need the data (tests) don't
// have to pull in JSX.
export * from './envelope'

// TriggerMessage renders the user-turn bubble for a GitHub-triggered chat: the
// XML-ish envelope (design: .quack/trigger-prompts-v2.md) as collapsible
// structured sections, permissions/deliverable/ask always visible, everything
// else collapsed. `content` that doesn't parse as an envelope (a plain typed
// message, or malformed input) renders exactly as it always has - the plain
// blue bubble, never a blank message (#667).
export function TriggerMessage({
  content,
  attachments,
  priorContents = [],
}: {
  content: string
  attachments?: ReactNode
  // This chat's earlier turns' raw envelope text, oldest first - lets the
  // <comments> section fold this turn's delta onto the running history
  // instead of rendering just what this one trigger saw.
  priorContents?: string[]
}) {
  const blocks = useMemo(() => parseEnvelope(content), [content])
  if (blocks) {
    return (
      <div className="flex justify-end mb-3">
        <div className="max-w-3xl w-full ml-auto bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tr-sm px-5 py-4 space-y-2.5">
          {blocks.map((b, i) => <EnvelopeBlockView key={i} block={b} priorContents={priorContents} />)}
        </div>
      </div>
    )
  }
  return (
    <div className="flex justify-end mb-3">
      <div className="max-w-2xl ml-auto">
        <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
          {attachments}
          {content}
        </div>
      </div>
    </div>
  )
}

function EnvelopeBlockView({ block, priorContents }: { block: EnvelopeBlock; priorContents: string[] }) {
  switch (block.kind) {
    case 'permissions': return <InfoLine label="Permissions" text={block.text} />
    case 'deliverable': return <InfoLine label="Deliverable" text={block.text} />
    case 'ask': return <AskSection block={block} />
    case 'comments': return <CommentsSection block={block} priorContents={priorContents} />
    case 'changed_files': return <ChangedFilesSection block={block} />
    case 'event': return <EventSection block={block} />
    case 'context': return <ContextSection block={block} />
    case 'unknown': return <UnknownSection block={block} />
  }
}

// InfoLine is the always-visible, one-line permissions/deliverable row - the
// two things a human scanning a run wants first, so they're never behind a click.
function InfoLine({ label, text }: { label: string; text: string }) {
  return (
    <div className="text-xs">
      <span className="font-semibold text-gray-500 dark:text-gray-400 mr-1">{label}:</span>
      <span className="text-gray-700 dark:text-gray-200">{text}</span>
    </div>
  )
}

// CollapsibleSection is the shared shell for every collapsed-by-default block
// - native <details>/<summary>, matching the disclosure pattern already used
// for tool calls/reasoning (AgentParts) and the DAG "Steps" toggle (TurnView),
// rather than a second collapse mechanism. Long content inside is separately
// height-locked with Expandable (below) so a big body can't wall off the page
// even once opened.
function CollapsibleSection({ summary, children }: { summary: ReactNode; children: ReactNode }) {
  return (
    <details className="rounded-lg border border-gray-200 dark:border-gray-700 not-prose">
      <summary className="cursor-pointer select-none px-3 py-1.5 text-xs font-medium text-gray-600 dark:text-gray-300 hover:text-gray-800 dark:hover:text-gray-100">
        {summary}
      </summary>
      <div className="px-3 pb-2.5 pt-1">{children}</div>
    </details>
  )
}

// RawFallback is the degrade-gracefully view for a block whose body didn't
// parse the way this section expects (bad JSON, missing child tags, an
// unknown tag) - the raw text, never dropped.
function RawFallback({ text }: { text: string }) {
  if (!text) return <span className="text-[11px] text-gray-400 dark:text-gray-500 italic">(empty)</span>
  return (
    <Expandable maxHeight={240} fade="from-gray-50 dark:from-gray-900">
      <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono text-[11px] text-gray-700 dark:text-gray-200">{text}</pre>
    </Expandable>
  )
}

function formatTimestamp(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

// AskSection - <issue>/<pull_request>: title as a heading, description as
// markdown. Always visible (not collapsible) - it's the thing being worked
// on - but a long description is height-locked (#746 item 8): a short one
// renders whole with no control, a long one collapses to its first lines
// with a Show more toggle, so it can't push everything else off screen.
function AskSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'ask' }> }) {
  return (
    <div>
      <h3 className="text-sm font-semibold text-gray-800 dark:text-gray-100">
        {block.number && <span className="text-gray-400 dark:text-gray-500 font-normal mr-1">#{block.number}</span>}
        {block.title || '(untitled)'}
      </h3>
      {block.description && (
        <Expandable maxHeight={140} fade="from-white dark:from-gray-800">
          <AssistantText text={block.description} />
        </Expandable>
      )}
    </div>
  )
}

// StatusBadge marks a delta comment's quack_status (edited/deleted) - a
// deleted comment's body reads identically to a live one otherwise, and
// treating a retracted comment as current is the exact failure this prevents.
function StatusBadge({ status }: { status: string }) {
  const deleted = status === 'deleted'
  return (
    <span
      className={`px-1 rounded text-[10px] font-medium uppercase tracking-wide ${
        deleted
          ? 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
          : 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
      }`}
    >
      {status}
    </span>
  )
}

// IncompleteHistoryNotice marks an accumulated comment list this client can't
// vouch for as complete - no seed turn is visible (a rehydrated store, or a
// chat opened after reaping), so what follows is only what's been captured
// since, not the issue's whole thread.
function IncompleteHistoryNotice() {
  return (
    <div className="mb-2 rounded border border-amber-200 dark:border-amber-800 bg-amber-50 dark:bg-amber-900/20 px-2 py-1 text-[11px] text-amber-800 dark:text-amber-300">
      Incomplete history - this client never saw the earlier comments, so the list below starts partway through the conversation.
    </div>
  )
}

// CommentsSection - the collapsed header always reports THIS turn's own
// count/delta (what the model actually saw, per envelope.go's commentsBlock);
// the expanded body renders the running history folded from this turn plus
// every earlier turn's delta (new appends, edited replaces by id, deleted
// removes - accumulateComments), not just this trigger's slice (#730).
// Expandable caps the opened thread's height so a long backlog doesn't wall
// off the message (#667 test case).
function CommentsSection({ block, priorContents }: { block: Extract<EnvelopeBlock, { kind: 'comments' }>; priorContents: string[] }) {
  const acc = useMemo(() => block.comments ? accumulateComments(priorContents, block) : undefined, [block, priorContents])
  return (
    <CollapsibleSection summary={commentsSummaryLabel(block)}>
      {acc ? (
        <>
          {!acc.complete && <IncompleteHistoryNotice />}
          {acc.comments.length > 0 ? (
            <Expandable maxHeight={360} fade="from-white dark:from-gray-800">
              <ul className="space-y-3">
                {acc.comments.map((c, i) => (
                  <li
                    key={c.id ?? i}
                    className={`border-l-2 pl-3 ${c.quackStatus === 'deleted' ? 'border-red-300 dark:border-red-800' : 'border-gray-200 dark:border-gray-700'}`}
                  >
                    <div className="flex items-center gap-2 text-[11px] text-gray-400 dark:text-gray-500 mb-0.5">
                      {c.author && <span className="font-medium text-gray-600 dark:text-gray-300">{c.author}</span>}
                      {c.createdAt && <span>{formatTimestamp(c.createdAt)}</span>}
                      {c.quackStatus && c.quackStatus !== 'new' && <StatusBadge status={c.quackStatus} />}
                    </div>
                    <div className={c.quackStatus === 'deleted' ? 'opacity-60 line-through decoration-red-400' : undefined}>
                      <AssistantText text={c.body} />
                    </div>
                  </li>
                ))}
              </ul>
            </Expandable>
          ) : <span className="text-[11px] text-gray-400 dark:text-gray-500 italic">no comments</span>}
        </>
      ) : <RawFallback text={block.raw} />}
    </CollapsibleSection>
  )
}

// ChangedFilesSection - collapsed, header is "N files, +A/-D"; expands to the
// per-file churn list.
function ChangedFilesSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'changed_files' }> }) {
  return (
    <CollapsibleSection summary={changedFilesSummaryLabel(block)}>
      {block.files ? (
        <Expandable maxHeight={320} fade="from-white dark:from-gray-800">
          <ul className="space-y-0.5 text-[11px] font-mono">
            {block.files.map((f, i) => (
              <li key={i} className="flex items-center gap-2">
                <span className="truncate text-gray-700 dark:text-gray-200">{f.filename}</span>
                <span className="ml-auto shrink-0 tabular-nums space-x-1.5">
                  {f.additions != null && <span className="text-green-600 dark:text-green-400">+{f.additions}</span>}
                  {f.deletions != null && <span className="text-red-500 dark:text-red-400">-{f.deletions}</span>}
                </span>
              </li>
            ))}
          </ul>
        </Expandable>
      ) : <RawFallback text={block.raw} />}
    </CollapsibleSection>
  )
}

// topLevelFields pulls the event JSON's own top-level PRIMITIVE fields
// (skipping nested objects/arrays, which stay in the full JSON body below) -
// #746 item 9: a wide pane rendering one "key: value" per line wastes the
// width it has; a grid puts related fields side by side instead.
function topLevelFields(pretty: string | null): [string, string][] {
  if (!pretty) return []
  try {
    const obj: unknown = JSON.parse(pretty)
    if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return []
    return Object.entries(obj as Record<string, unknown>)
      .filter(([, v]) => v == null || typeof v !== 'object')
      .map(([k, v]) => [k, String(v)])
  } catch {
    return []
  }
}

// EventSection - collapsed, header is the event name; expands to the
// top-level fields as a responsive grid (#746 item 9), then the full
// pretty-printed JSON below for anything nested. The JSON is routed through
// AssistantText's own ```json fence so it gets the same rehype-highlight
// syntax colouring as any other code block, rather than a second highlighter.
function EventSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'event' }> }) {
  const body = block.pretty ?? block.raw
  const fields = topLevelFields(block.pretty)
  return (
    <CollapsibleSection summary={<code className="font-mono">{block.name ?? 'event'}</code>}>
      {fields.length > 0 && (
        <dl className="grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-1.5 mb-2 not-prose">
          {fields.map(([k, v]) => (
            <div key={k} className="min-w-0">
              <dt className="text-[10px] uppercase tracking-wide text-gray-400 dark:text-gray-500">{k}</dt>
              <dd className="text-[11px] font-mono text-gray-700 dark:text-gray-200 truncate" title={v}>{v}</dd>
            </div>
          ))}
        </dl>
      )}
      <Expandable maxHeight={320} fade="from-white dark:from-gray-800">
        <AssistantText text={'```json\n' + body + '\n```'} />
      </Expandable>
    </CollapsibleSection>
  )
}

// ContextSection - collapsed, header is the file count; expands to filenames
// with the endpoint each came from.
function ContextSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'context' }> }) {
  const n = block.files.length
  return (
    <CollapsibleSection summary={`${n} file${n === 1 ? '' : 's'}`}>
      {n === 0 ? (
        <span className="text-[11px] text-gray-400 dark:text-gray-500 italic">no context files</span>
      ) : (
        <ul className="space-y-0.5 text-[11px] font-mono">
          {block.files.map((f, i) => (
            <li key={i} className="flex gap-2">
              <span className="text-gray-700 dark:text-gray-200 shrink-0">{f.name}</span>
              <span className="text-gray-400 dark:text-gray-500 truncate">{f.endpoint}</span>
            </li>
          ))}
        </ul>
      )}
    </CollapsibleSection>
  )
}

// UnknownSection - a block type this view doesn't recognise renders as a
// labelled collapsed section with its raw content rather than being dropped
// (#667's hardest requirement: a viewer silently missing part of the trigger
// is worse than an ugly one).
function UnknownSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'unknown' }> }) {
  return (
    <CollapsibleSection summary={<code className="font-mono">{`<${block.tag}>`}</code>}>
      <RawFallback text={block.raw} />
    </CollapsibleSection>
  )
}

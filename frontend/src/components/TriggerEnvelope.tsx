import { memo, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { AssistantText } from './AgentParts'
import { Expandable } from './Expandable'
import { ArtifactPanel } from './ArtifactPanel'
import { api } from '../api'
import {
  parseEnvelope,
  commentsSummaryLabel,
  changedFilesSummaryLabel,
  checksSummaryLabel,
  artifactsSummaryLabel,
  accumulateComments,
  type EnvelopeBlock,
  type ArtifactRow,
} from './envelope'

// Re-export the parser so import sites that only need the data (tests) don't
// have to pull in JSX.
export * from './envelope'

// Test-only render probe (PR #1300 review finding 2): counts every actual
// invocation of TriggerMessage's function body, so a perf test can pin
// memo(TriggerMessage) directly instead of inferring it from render timing
// (unreliable under jsdom - AssistantText's own useMemo chain already
// prevents most of the markdown re-parse cost the memo used to be gated on).
export const triggerMessageRenderProbe = { count: 0 }

// TriggerMessage renders the user-turn bubble for a GitHub-triggered chat: the
// XML-ish envelope (design: .quack/trigger-prompts-v2.md) as collapsible
// structured sections, permissions/deliverable/ask always visible, everything
// else collapsed. `content` that doesn't parse as an envelope (a plain typed
// message, or malformed input) renders exactly as it always has - the plain
// blue bubble, never a blank message (#667).
// Content is immutable for the life of a run but Chat.tsx re-renders this
// on every store notification (one per animation frame); memo + a stable
// `attachments` element keep re-renders from re-parsing the envelope markdown.
export const TriggerMessage = memo(function TriggerMessage({
  content,
  attachments,
  priorContents = [],
  chatId,
}: {
  content: string
  attachments?: ReactNode
  // This chat's earlier turns' raw envelope text, oldest first - lets the
  // <comments> section fold this turn's delta onto the running history
  // instead of rendering just what this one trigger saw.
  priorContents?: string[]
  // Present only for a real chat - gates whether an <artifacts> row can open
  // the artifact panel (needs a chat to look the artifact's owning node up in).
  chatId?: string
}) {
  triggerMessageRenderProbe.count++
  const blocks = useMemo(() => parseEnvelope(content), [content])
  // The artifact panel opens onto a NODE (resolved from the tapped row's
  // artifact id - see ArtifactsSection.openRow below), with that same
  // artifact id passed through as a focus hint so the panel shows the
  // TAPPED artifact as primary, not just whichever of the node's outputs
  // selectPrimaryOutput would otherwise pick (#1250 review). null means closed.
  const [openArtifact, setOpenArtifact] = useState<{ nodeId: string; artifactId: string } | null>(null)
  if (blocks) {
    return (
      <div className="flex justify-end mb-3">
        <div className="max-w-3xl w-full ml-auto bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tr-sm px-5 py-4 space-y-2.5">
          {blocks.map((b, i) => (
            <EnvelopeBlockView
              key={i}
              block={b}
              priorContents={priorContents}
              chatId={chatId}
              onOpenArtifact={(nodeId, artifactId) => setOpenArtifact({ nodeId, artifactId })}
            />
          ))}
        </div>
        {chatId && openArtifact && (
          <ArtifactPanel
            chatId={chatId}
            nodeId={openArtifact.nodeId}
            nodeAgent="Context"
            nodeTask=""
            focusArtifactId={openArtifact.artifactId}
            onClose={() => setOpenArtifact(null)}
          />
        )}
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
})

function EnvelopeBlockView({
  block,
  priorContents,
  chatId,
  onOpenArtifact,
}: {
  block: EnvelopeBlock
  priorContents: string[]
  chatId?: string
  onOpenArtifact: (nodeId: string, artifactId: string) => void
}) {
  switch (block.kind) {
    case 'permissions': return <InfoLine label="Permissions" text={block.text} />
    case 'deliverable': return <InfoLine label="Deliverable" text={block.text} />
    case 'ask': return <AskSection block={block} />
    case 'comments': return <CommentsSection block={block} priorContents={priorContents} />
    case 'changed_files': return <ChangedFilesSection block={block} />
    case 'checks': return <ChecksSection block={block} />
    case 'event': return <EventSection block={block} />
    case 'context': return <ContextSection block={block} />
    case 'artifacts': return <ArtifactsSection block={block} chatId={chatId} onOpenArtifact={onOpenArtifact} />
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
  if (!text) return <span className="text-[11px] text-gray-500 dark:text-gray-400 italic">(empty)</span>
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
        {block.number && <span className="text-gray-500 dark:text-gray-400 font-normal mr-1">#{block.number}</span>}
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
                    <div className="flex items-center gap-2 text-[11px] text-gray-500 dark:text-gray-400 mb-0.5">
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
          ) : <span className="text-[11px] text-gray-500 dark:text-gray-400 italic">no comments</span>}
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

// failingCheck reports whether a check's terminal state should read as a
// failure - the same rule checksBlock uses to build its own summary counts.
function failingCheck(c: { status: string; conclusion?: string }): boolean {
  return c.status === 'completed' && (c.conclusion === 'failure' || c.conclusion === 'timed_out')
}

// ChecksSection - collapsed, header is the backend's failing/pending/passing
// summary; expands to one monospace line per check, failing ones in the
// existing red danger token so they read at a glance.
function ChecksSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'checks' }> }) {
  return (
    <CollapsibleSection summary={checksSummaryLabel(block)}>
      {block.checks ? (
        <Expandable maxHeight={320} fade="from-white dark:from-gray-800">
          <ul className="space-y-0.5 text-[11px] font-mono">
            {block.checks.map((c, i) => {
              const failing = failingCheck(c)
              return (
                <li key={i} className="flex items-center gap-2">
                  <span className={`truncate ${failing ? 'text-red-500 dark:text-red-400' : 'text-gray-700 dark:text-gray-200'}`}>
                    {c.name}
                  </span>
                  <span className={`ml-auto shrink-0 ${failing ? 'text-red-500 dark:text-red-400 font-semibold' : 'text-gray-500 dark:text-gray-400'}`}>
                    {c.status}{c.conclusion ? ` ${c.conclusion}` : ''}
                  </span>
                </li>
              )
            })}
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
              <dt className="text-[10px] uppercase tracking-wide text-gray-500 dark:text-gray-400">{k}</dt>
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
        <span className="text-[11px] text-gray-500 dark:text-gray-400 italic">no context files</span>
      ) : (
        <ul className="space-y-0.5 text-[11px] font-mono">
          {block.files.map((f, i) => (
            <li key={i} className="flex gap-2">
              <span className="text-gray-700 dark:text-gray-200 shrink-0">{f.name}</span>
              <span className="text-gray-500 dark:text-gray-400 truncate">{f.endpoint}</span>
            </li>
          ))}
        </ul>
      )}
    </CollapsibleSection>
  )
}

// ARTIFACT_ICON_PATHS - one inline Material-Symbols-style path per id
// `kind:` prefix, distinguishing at a glance without a shared Icon
// component (chore/material-icons isn't merged yet - switch to it once it is).
const ARTIFACT_ICON_PATHS: Record<string, string> = {
  text: 'M6 2a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8l-6-6H6zm7 1.5L17.5 8H13V3.5zM8 13h8v1.5H8V13zm0 3h8v1.5H8V16zm0-6h4v1.5H8V10z',
  structured: 'M8 3H7a2 2 0 0 0-2 2v3a1 1 0 0 1-1 1H3v2h1a1 1 0 0 1 1 1v3a2 2 0 0 0 2 2h1v-2H7v-4a2 2 0 0 0-1-1.73A2 2 0 0 0 7 8V5h1V3zm8 0h1a2 2 0 0 1 2 2v3a1 1 0 0 0 1 1h1v2h-1a1 1 0 0 0-1 1v3a2 2 0 0 1-2 2h-1v-2h1v-4a2 2 0 0 1 1-1.73A2 2 0 0 1 17 8V5h-1V3z',
  image: 'M5 4a1 1 0 0 0-1 1v14a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1V5a1 1 0 0 0-1-1H5zm1 2h12v8.5l-3-3-4 4-2-2-3 3V6zm2 3a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z',
  bytes: 'M6 2a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8l-6-6H6zm7 1.5L17.5 8H13V3.5z',
}

function ArtifactIcon({ kindPrefix }: { kindPrefix: string }) {
  const path = ARTIFACT_ICON_PATHS[kindPrefix] ?? ARTIFACT_ICON_PATHS.bytes
  return (
    <svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor" aria-hidden="true" className="shrink-0 text-gray-500 dark:text-gray-400">
      <path d={path} />
    </svg>
  )
}

// ArtifactStatusChip maps an artifact's new/updated/unchanged status onto the
// same colour tokens ChangedFilesSection already uses for +/- churn - green
// for new (matches +additions), amber for updated (matches StatusBadge's
// edited), neutral gray for unchanged (no existing "unchanged" token).
function ArtifactStatusChip({ status }: { status: string }) {
  const cls =
    status === 'new'
      ? 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300'
      : status === 'updated'
        ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
        : 'bg-gray-100 text-gray-500 dark:bg-gray-800 dark:text-gray-400'
  return <span className={`px-1 rounded text-[10px] font-medium uppercase tracking-wide shrink-0 ${cls}`}>{status}</span>
}

// ArtifactsSection - <artifacts>: one compact row per artifact (icon, name,
// revision, status chip, summary), tapping a row opens that artifact in the
// artifact panel (#1250) rather than showing the raw XML that used to fall
// through to UnknownSection.
function ArtifactsSection({
  block,
  chatId,
  onOpenArtifact,
}: {
  block: Extract<EnvelopeBlock, { kind: 'artifacts' }>
  chatId?: string
  onOpenArtifact: (nodeId: string, artifactId: string) => void
}) {
  // Resolving a row to a node is one API round trip shared by every row in
  // this block - the artifact panel opens by node id, not artifact id (#1178
  // removed the id-based picker), so a tap looks up the tapped artifact's
  // owning node on demand rather than eagerly fetching for a block that's
  // usually never opened.
  const [pending, setPending] = useState<string | null>(null)
  const openRow = (row: ArtifactRow) => {
    if (!chatId || pending) return
    setPending(row.id)
    api.listChatArtifacts(chatId)
      .then(l => {
        const match = l.data?.find(a => a.name === row.id)
        if (match?.lineage?.node_id) onOpenArtifact(match.lineage.node_id, row.id)
      })
      .catch(() => {})
      .finally(() => setPending(null))
  }
  return (
    <CollapsibleSection summary={artifactsSummaryLabel(block)}>
      {block.items.length === 0 ? (
        <RawFallback text={block.raw} />
      ) : (
        <ul className="space-y-1">
          {block.items.map((row, i) => (
            <li key={i}>
              <button
                type="button"
                disabled={!chatId}
                onClick={() => openRow(row)}
                className="w-full flex items-start gap-2 px-1.5 py-1 rounded text-left text-[11px] hover:bg-gray-100 dark:hover:bg-gray-700/60 disabled:hover:bg-transparent disabled:cursor-default"
              >
                <ArtifactIcon kindPrefix={row.kindPrefix} />
                <span className="flex-1 min-w-0 flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
                  <span className="font-mono text-gray-700 dark:text-gray-200 break-all">{row.name}</span>
                  {row.revision != null && <span className="text-gray-500 dark:text-gray-400">rev {row.revision}</span>}
                  {row.status && <ArtifactStatusChip status={row.status} />}
                  <span className="text-gray-500 dark:text-gray-400 break-words basis-full sm:basis-auto">{row.summary}</span>
                </span>
              </button>
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

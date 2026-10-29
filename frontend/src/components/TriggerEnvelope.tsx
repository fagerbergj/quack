import { memo, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { AssistantText } from './AgentParts'
import { Expandable } from './Expandable'
import { ArtifactPanel } from './ArtifactPanel'
import { fieldLabel } from '../lib/json'
import { api } from '../api'
import { Icon } from './Icon'
import { parseA2uiActionText } from '../lib/a2ui'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Card } from './ui/card'
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

// A button press on an A2UI surface: the turn's text is machine JSON, so show what was
// sent as a pill, with the context the model receives one tap away.
function A2uiActionPill({ name, surfaceId, context }: { name: string; surfaceId: string; context: unknown }) {
  return (
    <div className="flex justify-end mb-3">
      <details className="max-w-full min-w-0 text-xs text-gray-600 dark:text-gray-300">
        <Card asChild className="rounded-full">
        <summary className="ml-auto flex w-fit max-w-full cursor-pointer list-none items-center gap-2 px-3 py-1 break-all">
          <Icon name="check_circle" className="w-3.5 h-3.5 shrink-0 text-blue-600 dark:text-blue-400" />
          <span>Submitted: <span className="font-medium text-gray-900 dark:text-gray-100">{name}</span> on {surfaceId}</span>
          <Icon name="expand_more" className="w-3.5 h-3.5 shrink-0" />
        </summary>
        </Card>
        <Card asChild variant="flat"><pre className="mt-1 overflow-x-auto p-2 font-mono text-2xs">{JSON.stringify(context ?? {}, null, 2)}</pre></Card>
      </details>
    </div>
  )
}

// Non-envelope content falls back to the plain bubble, never a blank message. Chat.tsx re-renders this every
// frame, so memo plus a stable `attachments` element keep it from re-parsing the envelope markdown.
export const TriggerMessage = memo(function TriggerMessage({
  content,
  attachments,
  priorContents = [],
  chatId,
}: {
  content: string
  attachments?: ReactNode
  // Earlier turns' raw envelopes, oldest first, so <comments> folds this delta onto the running history.
  priorContents?: string[]
  // Opening an <artifacts> row needs a chat to look up the artifact's owning node.
  chatId?: string
}) {
  const blocks = useMemo(() => parseEnvelope(content), [content])
  // The panel opens on a node; artifactId is a focus hint so the tapped artifact shows as primary.
  const [openArtifact, setOpenArtifact] = useState<{ nodeId: string; artifactId: string } | null>(null)
  const action = useMemo(() => parseA2uiActionText(content), [content])
  if (action) return <A2uiActionPill name={action.name} surfaceId={action.surfaceId} context={action.context} />
  if (blocks) {
    return (
      <div className="flex justify-end mb-3">
        <Card className="max-w-3xl w-full ml-auto rounded-2xl rounded-tr-sm px-5 py-4 space-y-3">
          {blocks.map((b, i) => (
            <EnvelopeBlockView
              key={i}
              block={b}
              priorContents={priorContents}
              chatId={chatId}
              onOpenArtifact={(nodeId, artifactId) => setOpenArtifact({ nodeId, artifactId })}
            />
          ))}
        </Card>
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

// Permissions and deliverable are what a reader wants first, so they're never behind a click.
function InfoLine({ label, text }: { label: string; text: string }) {
  return (
    <div className="text-xs">
      <span className="font-semibold text-gray-500 dark:text-gray-400 mr-1">{label}:</span>
      <span className="text-gray-700 dark:text-gray-200">{text}</span>
    </div>
  )
}

// Native <details>, the same disclosure used across the chat UI; bodies are height-locked separately with Expandable.
function CollapsibleSection({ summary, children }: { summary: ReactNode; children: ReactNode }) {
  return (
    // Full-bleed divided rows (an M3 list inside the card), not an outlined box per section; -mx-5/px-5 match the card's padding.
    <details className="-mx-5 px-5 !mb-0 border-t first:border-t-0 border-gray-200 dark:border-gray-700 not-prose">
      <summary className="cursor-pointer select-none py-3.5 medium:py-3 text-xs font-medium text-gray-600 dark:text-gray-300 hover:text-gray-800 dark:hover:text-gray-100">
        {summary}
      </summary>
      <div className="pb-3">{children}</div>
    </details>
  )
}

// A body that didn't parse as expected shows as raw text, never dropped.
function RawFallback({ text }: { text: string }) {
  if (!text) return <span className="text-2xs text-gray-500 dark:text-gray-400 italic">(empty)</span>
  return (
    <Expandable maxHeight={240} fade="from-muted">
      <Card asChild variant="flat"><pre className="p-2 overflow-x-auto whitespace-pre-wrap font-mono text-2xs">{text}</pre></Card>
    </Expandable>
  )
}

function formatTimestamp(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

// Always visible since it's the thing being worked on, but a long description is height-locked so it
// can't push everything else off screen.
function AskSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'ask' }> }) {
  return (
    <div>
      <h2 className="text-sm font-semibold text-gray-800 dark:text-gray-100">
        {block.number && <span className="text-gray-500 dark:text-gray-400 font-normal mr-1">#{block.number}</span>}
        {block.title || '(untitled)'}
      </h2>
      {block.description && (
        <Expandable maxHeight={140} fade="from-card">
          {/* A description's own headings sit under the card's title, so they render title-small, not prose's 20-24px. */}
          <div className="[&_:is(h1,h2,h3,h4)]:text-sm [&_:is(h1,h2,h3,h4)]:font-semibold [&_:is(h1,h2,h3,h4)]:mt-3 [&_:is(h1,h2,h3,h4)]:mb-1">
            <AssistantText text={block.description} />
          </div>
        </Expandable>
      )}
    </div>
  )
}

// A deleted comment's body reads like a live one; this badge keeps it from being taken as current.
function StatusBadge({ status }: { status: string }) {
  const deleted = status === 'deleted'
  return (
    <Badge tone={deleted ? 'red' : 'amber'}>{status}</Badge>
  )
}

function IncompleteHistoryNotice() {
  return (
    <div className="mb-2 rounded border border-amber-200 dark:border-amber-800 bg-amber-50 dark:bg-amber-900/20 px-2 py-1 text-2xs text-amber-800 dark:text-amber-300">
      Incomplete history - this client never saw the earlier comments, so the list below starts partway through the conversation.
    </div>
  )
}

// The header reports this turn's own delta (what the model saw); the body shows the running history
// folded across all turns.
function CommentsSection({ block, priorContents }: { block: Extract<EnvelopeBlock, { kind: 'comments' }>; priorContents: string[] }) {
  const acc = useMemo(() => block.comments ? accumulateComments(priorContents, block) : undefined, [block, priorContents])
  return (
    <CollapsibleSection summary={commentsSummaryLabel(block)}>
      {acc ? (
        <>
          {!acc.complete && <IncompleteHistoryNotice />}
          {acc.comments.length > 0 ? (
            <Expandable maxHeight={360} fade="from-card">
              <ul className="space-y-3">
                {acc.comments.map((c, i) => (
                  <li
                    key={c.id ?? i}
                    className={`border-l-2 pl-3 ${c.quackStatus === 'deleted' ? 'border-red-300 dark:border-red-800' : 'border-gray-200 dark:border-gray-700'}`}
                  >
                    <div className="flex items-center gap-2 text-2xs text-gray-500 dark:text-gray-400 mb-0.5">
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
          ) : <span className="text-2xs text-gray-500 dark:text-gray-400 italic">no comments</span>}
        </>
      ) : <RawFallback text={block.raw} />}
    </CollapsibleSection>
  )
}

function ChangedFilesSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'changed_files' }> }) {
  return (
    <CollapsibleSection summary={changedFilesSummaryLabel(block)}>
      {block.files ? (
        <Expandable maxHeight={320} fade="from-card">
          <ul className="space-y-0.5 text-2xs font-mono">
            {block.files.map((f, i) => (
              <li key={i} className="flex items-center gap-2">
                <span className="truncate text-gray-700 dark:text-gray-200">{f.filename}</span>
                <span className="ml-auto shrink-0 tabular-nums space-x-2">
                  {f.additions != null && <span className="text-green-700 dark:text-green-400">+{f.additions}</span>}
                  {f.deletions != null && <span className="text-red-700 dark:text-red-400">-{f.deletions}</span>}
                </span>
              </li>
            ))}
          </ul>
        </Expandable>
      ) : <RawFallback text={block.raw} />}
    </CollapsibleSection>
  )
}

// Same rule the backend's checksBlock uses for its summary counts.
function failingCheck(c: { status: string; conclusion?: string }): boolean {
  return c.status === 'completed' && (c.conclusion === 'failure' || c.conclusion === 'timed_out')
}

function ChecksSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'checks' }> }) {
  return (
    <CollapsibleSection summary={checksSummaryLabel(block)}>
      {block.checks ? (
        <Expandable maxHeight={320} fade="from-card">
          <ul className="space-y-0.5 text-2xs font-mono">
            {block.checks.map((c, i) => {
              const failing = failingCheck(c)
              return (
                <li key={i} className="flex items-center gap-2">
                  <span className={`truncate ${failing ? 'text-red-700 dark:text-red-400' : 'text-gray-700 dark:text-gray-200'}`}>
                    {c.name}
                  </span>
                  <span className={`ml-auto shrink-0 ${failing ? 'text-red-700 dark:text-red-400 font-semibold' : 'text-gray-500 dark:text-gray-400'}`}>
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

// Primitive top-level fields only; nested values stay in the full JSON body below.
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

// The JSON goes through AssistantText's ```json fence to reuse its highlighter rather than add a second one.
function EventSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'event' }> }) {
  const body = block.pretty ?? block.raw
  const fields = topLevelFields(block.pretty)
  return (
    <CollapsibleSection summary={<code className="font-mono">{block.name ?? 'event'}</code>}>
      {fields.length > 0 && (
        <dl className="grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-2 mb-2 not-prose">
          {fields.map(([k, v]) => (
            <div key={k} className="min-w-0">
              <dt className="text-2xs text-gray-500 dark:text-gray-400">{fieldLabel(k)}</dt>
              <dd className="text-2xs font-mono text-gray-700 dark:text-gray-200 truncate" title={v}>{v}</dd>
            </div>
          ))}
        </dl>
      )}
      <Expandable maxHeight={320} fade="from-card">
        <AssistantText text={'```json\n' + body + '\n```'} />
      </Expandable>
    </CollapsibleSection>
  )
}

function ContextSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'context' }> }) {
  const n = block.files.length
  return (
    <CollapsibleSection summary={`${n} file${n === 1 ? '' : 's'}`}>
      {n === 0 ? (
        <span className="text-2xs text-gray-500 dark:text-gray-400 italic">no context files</span>
      ) : (
        <ul className="space-y-0.5 text-2xs font-mono">
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

// One inline path per id `kind:` prefix.
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

// Reuses the churn colours: green new, amber updated (as StatusBadge's edited), gray otherwise.
function ArtifactStatusChip({ status }: { status: string }) {
  return <Badge tone={status === 'new' ? 'green' : status === 'updated' ? 'amber' : 'gray'}>{status}</Badge>
}

function ArtifactsSection({
  block,
  chatId,
  onOpenArtifact,
}: {
  block: Extract<EnvelopeBlock, { kind: 'artifacts' }>
  chatId?: string
  onOpenArtifact: (nodeId: string, artifactId: string) => void
}) {
  // The panel opens by node id, so a tap looks up the owning node on demand (blank for dispatch's run input).
  const [pending, setPending] = useState<string | null>(null)
  const openRow = (row: ArtifactRow) => {
    if (!chatId || pending) return
    setPending(row.id)
    api.listChatArtifacts(chatId)
      .then(l => {
        const match = l.data?.find(a => a.name === row.id)
        if (match) onOpenArtifact(match.lineage?.node_id ?? '', row.id)
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
              <Button
                variant="ghost"
                size="inline"
                disabled={!chatId}
                onClick={() => openRow(row)}
                className="h-auto w-full items-start justify-start gap-2 whitespace-normal px-2 py-1 text-left text-2xs font-normal disabled:pointer-events-auto disabled:cursor-default disabled:opacity-100 disabled:hover:bg-transparent"
              >
                <ArtifactIcon kindPrefix={row.kindPrefix} />
                <span className="flex-1 min-w-0 flex flex-wrap items-center gap-x-2 gap-y-0.5">
                  <span className="font-mono text-gray-700 dark:text-gray-200 break-all">{row.name}</span>
                  {row.revision != null && <span className="text-gray-500 dark:text-gray-400">rev {row.revision}</span>}
                  {row.status && <ArtifactStatusChip status={row.status} />}
                  <span className="text-gray-500 dark:text-gray-400 break-words basis-full sm:basis-auto">{row.summary}</span>
                </span>
              </Button>
            </li>
          ))}
        </ul>
      )}
    </CollapsibleSection>
  )
}

// An unrecognised block shows raw rather than dropped: an ugly view beats silently missing part of a trigger.
function UnknownSection({ block }: { block: Extract<EnvelopeBlock, { kind: 'unknown' }> }) {
  return (
    <CollapsibleSection summary={<code className="font-mono">{`<${block.tag}>`}</code>}>
      <RawFallback text={block.raw} />
    </CollapsibleSection>
  )
}

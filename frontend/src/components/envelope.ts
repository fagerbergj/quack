import { tryParseJSON } from '../lib/json'

// Hand-rolled, not DOMParser: envelope content is seeded verbatim, not XML-escaped, so a literal `<` would
// break a real parser. Best-effort matching degrades instead of blanking the message.

import { str, num } from './toolFormat'

interface Comment {
  id?: string
  createdAt?: string
  author?: string
  body: string
  // Delta mode only: a deleted comment's body reads like a live one, so this is the only signal.
  quackStatus?: string
}

interface ChangedFile {
  filename: string
  additions?: number
  deletions?: number
  status?: string
}

interface ContextFile {
  name: string
  endpoint: string
}

interface CheckRun {
  name: string
  status: string
  conclusion?: string
}

export interface ArtifactRow {
  id: string
  // Drives the row icon; a malformed id with no prefix falls back to 'bytes'.
  kindPrefix: string
  name: string
  revision?: number
  status?: string
  summary: string
}

export type EnvelopeBlock =
  | { kind: 'permissions'; text: string }
  | { kind: 'deliverable'; text: string }
  | { kind: 'ask'; askKind: 'issue' | 'pull_request'; number?: string; title: string; description: string }
  | { kind: 'comments'; total?: number; added?: number; edited?: number; deleted?: number; comments: Comment[] | null; raw: string }
  | { kind: 'changed_files'; count?: number; additions?: number; deletions?: number; files: ChangedFile[] | null; raw: string }
  | { kind: 'checks'; count?: number; summary?: string; checks: CheckRun[] | null; raw: string }
  | { kind: 'event'; name?: string; pretty: string | null; raw: string }
  | { kind: 'context'; dir?: string; files: ContextFile[] }
  | { kind: 'artifacts'; items: ArtifactRow[]; raw: string }
  | { kind: 'unknown'; tag: string; attrs: Record<string, string>; raw: string }

// Every envelope carries one of these; a plain message that merely starts with "<" matches none.
const ENVELOPE_MARKERS = new Set(['permissions', 'deliverable', 'event'])

interface RawBlock {
  tag: string
  attrs: Record<string, string>
  content: string
}

function parseAttrs(attrsStr: string): Record<string, string> {
  const attrs: Record<string, string> = {}
  const re = /([\w-]+)="([^"]*)"/g
  let m: RegExpExecArray | null
  while ((m = re.exec(attrsStr))) attrs[m[1]] = m[2]
  return attrs
}

// Pairs each tag with its first matching close tag; stray text between tags is skipped, not rejected,
// so content that isn't strict XML never fails closed.
function parseTopLevel(src: string): RawBlock[] {
  const blocks: RawBlock[] = []
  const tagRe = /<([a-zA-Z][\w-]*)((?:\s+[\w-]+="[^"]*")*)\s*(\/)?>/g
  let idx = 0
  while (idx < src.length) {
    tagRe.lastIndex = idx
    const m = tagRe.exec(src)
    if (!m) break
    const [full, tag, attrsStr, selfClose] = m
    const attrs = parseAttrs(attrsStr)
    const afterOpen = m.index + full.length
    if (selfClose) {
      blocks.push({ tag, attrs, content: '' })
      idx = afterOpen
      continue
    }
    const closeTag = `</${tag}>`
    const closeIdx = src.indexOf(closeTag, afterOpen)
    if (closeIdx === -1) {
      // Unterminated: take the rest of the string as this block's content and stop.
      blocks.push({ tag, attrs, content: src.slice(afterOpen) })
      break
    }
    blocks.push({ tag, attrs, content: src.slice(afterOpen, closeIdx) })
    idx = closeIdx + closeTag.length
  }
  return blocks
}

// Blocks may arrive wrapped in <trigger> or flat; both parse the same.
function flattenTrigger(blocks: RawBlock[]): RawBlock[] {
  const out: RawBlock[] = []
  for (const b of blocks) {
    if (b.tag === 'trigger') out.push(...flattenTrigger(parseTopLevel(b.content)))
    else out.push(b)
  }
  return out
}

function extractChildTag(src: string, tag: string): string | null {
  const open = `<${tag}>`
  const close = `</${tag}>`
  const start = src.indexOf(open)
  if (start === -1) return null
  const end = src.indexOf(close, start + open.length)
  if (end === -1) return null
  return src.slice(start + open.length, end)
}

function toComment(item: unknown): Comment {
  const user = item && typeof item === 'object' ? (item as Record<string, unknown>).user : undefined
  return {
    id: str(item, 'id') ?? (num(item, 'id') != null ? String(num(item, 'id')) : undefined),
    createdAt: str(item, 'created_at'),
    author: str(user, 'login'),
    body: str(item, 'body') ?? '',
    quackStatus: str(item, 'quack_status'),
  }
}

function toChangedFile(item: unknown): ChangedFile {
  return {
    filename: str(item, 'filename') ?? str(item, 'name') ?? str(item, 'path') ?? '(unknown file)',
    additions: num(item, 'additions'),
    deletions: num(item, 'deletions'),
    status: str(item, 'status'),
  }
}

function toAskBlock(b: RawBlock): EnvelopeBlock {
  const title = extractChildTag(b.content, 'title')
  const description = extractChildTag(b.content, 'description')
  return {
    kind: 'ask',
    askKind: b.tag as 'issue' | 'pull_request',
    number: b.attrs.number,
    title: title?.trim() ?? '',
    // Neither child tag found: fall back to the raw block so nothing silently disappears.
    description: description != null ? description.trim() : (title == null ? b.content.trim() : ''),
  }
}

function toCommentsBlock(b: RawBlock): EnvelopeBlock {
  const raw = b.content.trim()
  const parsed = tryParseJSON(raw)
  const comments = Array.isArray(parsed) ? parsed.map(toComment) : null
  return {
    kind: 'comments',
    total: numAttr(b.attrs.count),
    added: numAttr(b.attrs.new),
    edited: numAttr(b.attrs.edited),
    deleted: numAttr(b.attrs.deleted),
    comments,
    raw,
  }
}

function toChangedFilesBlock(b: RawBlock): EnvelopeBlock {
  const raw = b.content.trim()
  const parsed = tryParseJSON(raw)
  const files = Array.isArray(parsed) ? parsed.map(toChangedFile) : null
  return {
    kind: 'changed_files',
    count: numAttr(b.attrs.count),
    additions: numAttr(b.attrs.additions),
    deletions: numAttr(b.attrs.deletions),
    files,
    raw,
  }
}

function toEventBlock(b: RawBlock): EnvelopeBlock {
  const raw = b.content.trim()
  const parsed = tryParseJSON(raw)
  return {
    kind: 'event',
    name: b.attrs.name,
    pretty: parsed !== undefined ? JSON.stringify(parsed, null, 2) : null,
    raw,
  }
}

function toEnvelopeBlock(b: RawBlock): EnvelopeBlock {
  switch (b.tag) {
    case 'permissions':
      return { kind: 'permissions', text: b.content.trim() }
    case 'deliverable':
      return { kind: 'deliverable', text: b.content.trim() }
    case 'issue':
    case 'pull_request':
      return toAskBlock(b)
    case 'comments':
      return toCommentsBlock(b)
    case 'changed_files':
      return toChangedFilesBlock(b)
    case 'checks': {
      const raw = b.content.trim()
      return {
        kind: 'checks',
        count: numAttr(b.attrs.count),
        summary: b.attrs.summary,
        checks: parseChecks(raw),
        raw,
      }
    }
    case 'event':
      return toEventBlock(b)
    case 'context': {
      const files = parseTopLevel(b.content)
        .filter(c => c.tag === 'file')
        .map(c => ({ name: c.attrs.name ?? '(unnamed)', endpoint: c.content.trim() }))
      return { kind: 'context', dir: b.attrs.dir, files }
    }
    case 'artifacts': {
      const raw = b.content.trim()
      const items = parseTopLevel(b.content)
        .filter(c => c.tag === 'artifact')
        .map(c => toArtifactRow(c))
      return { kind: 'artifacts', items, raw }
    }
    default:
      return { kind: 'unknown', tag: b.tag, attrs: b.attrs, raw: b.content.trim() }
  }
}

// Splits on the LAST ": " since a job name can contain one ("test: unit (node 18)") but the status never does.
function parseCheckLine(line: string): CheckRun | null {
  const idx = line.lastIndexOf(': ')
  if (idx === -1) return null
  const name = line.slice(0, idx).trim()
  const rest = line.slice(idx + 2).trim()
  if (!name || !rest) return null
  const [status, ...conclusionParts] = rest.split(/\s+/)
  if (status === 'completed') {
    const conclusion = conclusionParts.join(' ')
    return conclusion ? { name, status, conclusion } : null
  }
  return { name, status: rest }
}

// One unparseable line sends the whole block to raw rather than silently dropping checks.
function parseChecks(raw: string): CheckRun[] | null {
  const lines = raw.split('\n').map(l => l.trim()).filter(Boolean)
  if (lines.length === 0) return null
  const checks: CheckRun[] = []
  for (const line of lines) {
    const c = parseCheckLine(line)
    if (!c) return null
    checks.push(c)
  }
  return checks
}

// Splits "kind:instance"; an id with no colon falls back to 'bytes' with the whole id as the name.
function toArtifactRow(b: RawBlock): ArtifactRow {
  const id = b.attrs.id ?? ''
  const i = id.indexOf(':')
  const kindPrefix = i >= 0 ? id.slice(0, i) : 'bytes'
  const name = i >= 0 ? id.slice(i + 1) : id
  return {
    id,
    kindPrefix,
    name,
    revision: numAttr(b.attrs.revision),
    status: b.attrs.status,
    summary: b.content.trim(),
  }
}

function numAttr(v: string | undefined): number | undefined {
  if (v == null) return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

// Null for a plain message or any parse failure; callers then render `raw` as-is.
export function parseEnvelope(raw: string): EnvelopeBlock[] | null {
  try {
    const trimmed = raw.trim()
    if (!trimmed.startsWith('<')) return null
    const rawBlocks = flattenTrigger(parseTopLevel(trimmed))
    if (rawBlocks.length === 0) return null
    if (!rawBlocks.some(b => ENVELOPE_MARKERS.has(b.tag))) return null
    return rawBlocks.map(toEnvelopeBlock)
  } catch {
    return null
  }
}

type CommentsBlock = Extract<EnvelopeBlock, { kind: 'comments' }>

function commentsBlockOf(content: string): CommentsBlock | undefined {
  return parseEnvelope(content)?.find((b): b is CommentsBlock => b.kind === 'comments')
}

// A full first-load snapshot carries no new/edited/deleted attrs; a resume delta does.
function isSeed(b: CommentsBlock): boolean {
  return b.added == null && b.edited == null && b.deleted == null
}

export interface AccumulatedComments {
  comments: Comment[]
  // False when the earliest visible turn is itself a delta, so the list is not the issue's whole history.
  complete: boolean
}

// Replays the server's new/edited/deleted rule (envelope.go's diffSnapshots) over `priorContents`
// (oldest first) then `current`.
export function accumulateComments(priorContents: string[], current: CommentsBlock): AccumulatedComments {
  const blocks: CommentsBlock[] = []
  for (const c of priorContents) {
    const b = commentsBlockOf(c)
    if (b) blocks.push(b)
  }
  blocks.push(current)

  const byId = new Map<string, Comment>()
  const order: string[] = []
  let anonymous = 0
  for (const b of blocks) {
    for (const c of b.comments ?? []) {
      const id = c.id ?? `_${anonymous++}`
      if (c.quackStatus === 'deleted') {
        // A comment never seen alive is kept, marked deleted, rather than dropped: `complete` already
        // flags the gap, so don't lose data the delta carried.
        if (byId.has(id)) {
          byId.delete(id)
          order.splice(order.indexOf(id), 1)
        } else {
          byId.set(id, c)
          order.push(id)
        }
        continue
      }
      if (!byId.has(id)) order.push(id)
      byId.set(id, c)
    }
  }
  return { comments: order.map(id => byId.get(id)!), complete: isSeed(blocks[0]) }
}

export function commentsSummaryLabel(b: Extract<EnvelopeBlock, { kind: 'comments' }>): string {
  if (b.added != null || b.edited != null || b.deleted != null) {
    return `${b.added ?? 0} new, ${b.edited ?? 0} edited, ${b.deleted ?? 0} deleted`
  }
  const n = b.total ?? b.comments?.length ?? 0
  return `${n} comment${n === 1 ? '' : 's'}`
}

export function changedFilesSummaryLabel(b: Extract<EnvelopeBlock, { kind: 'changed_files' }>): string {
  const n = b.count ?? b.files?.length ?? 0
  const add = b.additions ?? 0
  const del = b.deletions ?? 0
  return `${n} file${n === 1 ? '' : 's'}, +${add}/-${del}`
}

export function artifactsSummaryLabel(b: Extract<EnvelopeBlock, { kind: 'artifacts' }>): string {
  const n = b.items.length
  return `${n} artifact${n === 1 ? '' : 's'}`
}

// The backend sends a summary attr with every non-empty block; the count is the fallback.
export function checksSummaryLabel(b: Extract<EnvelopeBlock, { kind: 'checks' }>): string {
  if (b.summary) return `checks: ${b.summary}`
  const n = b.count ?? b.checks?.length ?? 0
  return `${n} check${n === 1 ? '' : 's'}`
}

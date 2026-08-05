// Parses the GitHub trigger envelope (design: .quack/trigger-prompts-v2.md) - an
// XML-ish wrapper around GitHub's own JSON, seeded verbatim. Hand-rolled rather
// than DOMParser: the content inside <title>/<description>/JSON blocks is NOT
// XML-escaped (seeded verbatim per spec), so a real XML parser would choke on
// any body containing a literal `<`. Best-effort tag matching degrades instead
// of throwing - never blank the message over one malformed block (#667).
//
// No JSX here so this stays trivially testable; rendering lives in TriggerEnvelope.tsx.

import { str, num } from './toolFormat'

export interface Comment {
  id?: string
  createdAt?: string
  author?: string
  body: string
  // quack_status (delta mode only): "new" | "edited" | "deleted" - a retracted
  // comment's body reads identically to a live one, so this is the only signal.
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

export type EnvelopeBlock =
  | { kind: 'permissions'; text: string }
  | { kind: 'deliverable'; text: string }
  | { kind: 'ask'; askKind: 'issue' | 'pull_request'; number?: string; title: string; description: string }
  | { kind: 'comments'; total?: number; added?: number; edited?: number; deleted?: number; comments: Comment[] | null; raw: string }
  | { kind: 'changed_files'; count?: number; additions?: number; deletions?: number; files: ChangedFile[] | null; raw: string }
  | { kind: 'event'; name?: string; pretty: string | null; raw: string }
  | { kind: 'context'; dir?: string; files: ContextFile[] }
  | { kind: 'unknown'; tag: string; attrs: Record<string, string>; raw: string }

// The tags that mark this string as an envelope rather than a plain chat
// message - present on every step in the design doc, so any one of them is a
// reliable signal. A plain user message that happens to start with "<" (rare,
// but possible free text) won't match any of these and falls back untouched.
const ENVELOPE_MARKERS = new Set(['permissions', 'deliverable', 'event'])

interface RawBlock {
  tag: string
  attrs: Record<string, string>
  content: string
}

// parseAttrs reads double-quoted `key="value"` pairs off a tag's attribute string.
function parseAttrs(attrsStr: string): Record<string, string> {
  const attrs: Record<string, string> = {}
  const re = /([\w-]+)="([^"]*)"/g
  let m: RegExpExecArray | null
  while ((m = re.exec(attrsStr))) attrs[m[1]] = m[2]
  return attrs
}

// parseTopLevel walks `src` left to right, matching one tag at a time and
// pairing it with its first matching close tag. Stray text between tags
// (whitespace, or anything a mismatched/unterminated tag left behind) is
// skipped rather than rejected - the whole point is not to fail closed on
// content that isn't strictly valid XML.
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

// flattenTrigger inlines any <trigger> wrapper's children as top-level blocks,
// so the parser doesn't care whether permissions/deliverable/event/context
// arrive wrapped or flat.
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

function tryParseJSON(text: string): unknown | undefined {
  const trimmed = text.trim()
  if (!trimmed) return undefined
  try {
    return JSON.parse(trimmed)
  } catch {
    return undefined
  }
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

function toEnvelopeBlock(b: RawBlock): EnvelopeBlock {
  switch (b.tag) {
    case 'permissions':
      return { kind: 'permissions', text: b.content.trim() }
    case 'deliverable':
      return { kind: 'deliverable', text: b.content.trim() }
    case 'issue':
    case 'pull_request': {
      const title = extractChildTag(b.content, 'title')
      const description = extractChildTag(b.content, 'description')
      return {
        kind: 'ask',
        askKind: b.tag,
        number: b.attrs.number,
        title: title?.trim() ?? '',
        // Neither child tag found (malformed) - fall back to the raw block so
        // nothing silently disappears.
        description: description != null ? description.trim() : (title == null ? b.content.trim() : ''),
      }
    }
    case 'comments': {
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
    case 'changed_files': {
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
    case 'event': {
      const raw = b.content.trim()
      const parsed = tryParseJSON(raw)
      return {
        kind: 'event',
        name: b.attrs.name,
        pretty: parsed !== undefined ? JSON.stringify(parsed, null, 2) : null,
        raw,
      }
    }
    case 'context': {
      const files = parseTopLevel(b.content)
        .filter(c => c.tag === 'file')
        .map(c => ({ name: c.attrs.name ?? '(unnamed)', endpoint: c.content.trim() }))
      return { kind: 'context', dir: b.attrs.dir, files }
    }
    default:
      return { kind: 'unknown', tag: b.tag, attrs: b.attrs, raw: b.content.trim() }
  }
}

function numAttr(v: string | undefined): number | undefined {
  if (v == null) return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

// parseEnvelope returns the ordered top-level blocks of a GitHub trigger
// envelope, or null when `raw` doesn't look like one (a plain chat message,
// or - if anything goes wrong parsing it - malformed input). Callers fall
// back to rendering `raw` as-is on null.
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

// commentsBlockOf parses one turn's raw envelope content and returns its own
// <comments> block, or undefined if the turn has none / isn't an envelope.
function commentsBlockOf(content: string): CommentsBlock | undefined {
  return parseEnvelope(content)?.find((b): b is CommentsBlock => b.kind === 'comments')
}

// isSeed reports whether a <comments> block is the full first-load snapshot
// (envelope.go's commentsBlock: no new/edited/deleted attrs) rather than a
// resume delta.
function isSeed(b: CommentsBlock): boolean {
  return b.added == null && b.edited == null && b.deleted == null
}

export interface AccumulatedComments {
  comments: Comment[]
  // False when the earliest turn this client can see is itself a delta (no
  // seed in the visible window - a rehydrated store, or a chat opened after
  // reaping) - the list below is everything captured so far, not the issue's
  // whole history.
  complete: boolean
}

// accumulateComments folds a chat's <comments> blocks (`priorContents` oldest
// first, `current` last) into one running history, replaying the same
// new/edited/deleted rule the server applied when it built each delta
// (envelope.go's diffSnapshots) rather than re-sending anything to the model.
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
        // A comment already in the running history is removed outright. One
        // this client never saw alive (its own removal is the first record of
        // it - no seed in the visible window) is kept and marked deleted
        // instead of silently vanishing: the incompleteness is what `complete`
        // is for, not a reason to drop data this delta actually carried.
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

// commentsSummaryLabel is the collapsed header for a <comments> block: a plain
// count, or a new/edited/deleted breakdown when those delta attributes are present.
export function commentsSummaryLabel(b: Extract<EnvelopeBlock, { kind: 'comments' }>): string {
  if (b.added != null || b.edited != null || b.deleted != null) {
    return `${b.added ?? 0} new, ${b.edited ?? 0} edited, ${b.deleted ?? 0} deleted`
  }
  const n = b.total ?? b.comments?.length ?? 0
  return `${n} comment${n === 1 ? '' : 's'}`
}

// changedFilesSummaryLabel is the collapsed header for a <changed_files> block.
export function changedFilesSummaryLabel(b: Extract<EnvelopeBlock, { kind: 'changed_files' }>): string {
  const n = b.count ?? b.files?.length ?? 0
  const add = b.additions ?? 0
  const del = b.deletions ?? 0
  return `${n} file${n === 1 ? '' : 's'}, +${add}/-${del}`
}

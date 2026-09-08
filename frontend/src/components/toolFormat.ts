// Small formatting helpers for rendering tool calls.

// summarizeArgs picks a representative arg to show beside the tool name in the
// collapsed summary, so a call is identifiable without expanding it. Ordered by
// specificity: a file path / command is more telling than a bare dir.
export function summarizeArgs(args: Record<string, unknown>): string {
  for (const key of ['query', 'url', 'path', 'command', 'message', 'id', 'q']) {
    const v = args[key]
    if (typeof v === 'string' && v) return JSON.stringify(v)
  }
  return ''
}

// previewLine collapses to a single-line preview for thinking blocks (#385).
// Now prefers sentence boundaries (#959) to avoid mid-word cuts in folded blocks.
export function previewLine(text: string, max = 80): string {
  const oneLine = text.replace(/\s+/g, ' ').trim()
  if (oneLine.length <= max) return oneLine
  const sentence = oneLine.slice(0, max * 2).match(/^.{10,}?[.!?](?=\s|$)/)
  if (sentence) return sentence[0]
  const cut = oneLine.slice(0, max)
  const lastSpace = cut.lastIndexOf(' ')
  return (lastSpace > max * 0.6 ? cut.slice(0, lastSpace) : cut) + '…'
}

// fmtTokenCount compacts a token count for tight spaces (a context meter, a
// compaction row): thousands round to the nearest K, smaller counts show as-is.
export function fmtTokenCount(n: number): string {
  return n >= 1000 ? `${Math.round(n / 1000)}K` : String(n)
}

// TOOL_VERBS labels the compact live-status line's tool call (#725) with a
// present-participle verb; anything unmapped just shows its raw name.
const TOOL_VERBS: Record<string, string> = {
  edit_file: 'editing', write_file: 'writing', read_file: 'reading', delete_path: 'deleting',
  run_command: 'running', list_dir: 'listing', glob: 'searching', grep: 'searching',
  web_search: 'searching', web_fetch: 'fetching', git_commit: 'committing', git_diff: 'diffing',
  git_push: 'pushing', git_log: 'reading', git_status: 'checking', git_branch: 'checking',
}

// toolActionLine is the one-line "<verb> <target>" summary of a tool call -
// the live-status line's most-recent-tool-call display.
export function toolActionLine(name: string, args: Record<string, unknown>): string {
  const verb = TOOL_VERBS[name] ?? name
  const target = summarizeArgs(args).replace(/^"|"$/g, '')
  return target ? `${verb} ${target}` : verb
}

// toolFailed reports whether a completed tool call's result carries an error -
// the compact summary line's status icon (✗ vs ✓, #385) keys off this rather
// than any particular tool's own result shape.
export function toolFailed(result: unknown): boolean {
  return !!(result && typeof result === 'object' && 'error' in (result as Record<string, unknown>))
}

// prettyJSON renders a value as indented JSON, falling back to String on cycles.
export function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

// str reads a string field off a loosely-typed args/result bag, or undefined.
export function str(bag: unknown, key: string): string | undefined {
  if (bag && typeof bag === 'object') {
    const v = (bag as Record<string, unknown>)[key]
    if (typeof v === 'string') return v
  }
  return undefined
}

// num reads a number field off a loosely-typed args/result bag, or undefined.
export function num(bag: unknown, key: string): number | undefined {
  if (bag && typeof bag === 'object') {
    const v = (bag as Record<string, unknown>)[key]
    if (typeof v === 'number') return v
  }
  return undefined
}

// bool reads a boolean field off a loosely-typed args/result bag, or undefined.
export function bool(bag: unknown, key: string): boolean | undefined {
  if (bag && typeof bag === 'object') {
    const v = (bag as Record<string, unknown>)[key]
    if (typeof v === 'boolean') return v
  }
  return undefined
}

// ── diff model ───────────────────────────────────────────────────────────────

type DiffType = 'add' | 'remove' | 'context' | 'meta'
export interface DiffLine { type: DiffType; text: string }

// lineDiff computes a minimal line-level diff of old → new via an LCS walk. It's
// the flagship of edit_file rendering: the tool's `old`/`new` strings become a
// before→after diff (removed lines red, added lines green). Deterministic and
// dependency-free - no diff library. Empty inputs yield no lines.
export function lineDiff(oldStr: string, newStr: string): DiffLine[] {
  if (oldStr === '' && newStr === '') return []
  const a = oldStr.split('\n')
  const b = newStr.split('\n')
  const m = a.length
  const n = b.length
  // lcs[i][j] = length of the longest common subsequence of a[i:] and b[j:].
  const lcs: number[][] = Array.from({ length: m + 1 }, () => new Array<number>(n + 1).fill(0))
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    }
  }
  const out: DiffLine[] = []
  let i = 0
  let j = 0
  while (i < m && j < n) {
    if (a[i] === b[j]) {
      out.push({ type: 'context', text: a[i] })
      i++
      j++
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      out.push({ type: 'remove', text: a[i] })
      i++
    } else {
      out.push({ type: 'add', text: b[j] })
      j++
    }
  }
  while (i < m) { out.push({ type: 'remove', text: a[i] }); i++ }
  while (j < n) { out.push({ type: 'add', text: b[j] }); j++ }
  return out
}

// parseUnifiedDiff classifies the lines of a git-style unified diff string so
// git_diff renders coloured instead of as a raw blob. Leading +/- mark add/remove
// (but +++/--- file headers and @@ hunks are meta); everything else is context.
export function parseUnifiedDiff(diff: string): DiffLine[] {
  if (!diff) return []
  return diff.split('\n').map((text): DiffLine => {
    if (text.startsWith('+++') || text.startsWith('---')) return { type: 'meta', text }
    if (text.startsWith('@@') || text.startsWith('diff ') || text.startsWith('index ')) return { type: 'meta', text }
    if (text.startsWith('+')) return { type: 'add', text }
    if (text.startsWith('-')) return { type: 'remove', text }
    return { type: 'context', text }
  })
}

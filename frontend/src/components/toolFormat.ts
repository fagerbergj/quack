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

// previewLine collapses text to a single-line, length-capped preview — the
// collapsed-state summary for a thinking block (#385: a scannable one-liner
// beside the "Thought" label, not a wall of reasoning, until expanded).
export function previewLine(text: string, max = 80): string {
  const oneLine = text.replace(/\s+/g, ' ').trim()
  return oneLine.length > max ? oneLine.slice(0, max) + '…' : oneLine
}

// toolFailed reports whether a completed tool call's result carries an error —
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

export type DiffType = 'add' | 'remove' | 'context' | 'meta'
export interface DiffLine { type: DiffType; text: string }

// lineDiff computes a minimal line-level diff of old → new via an LCS walk. It's
// the flagship of edit_file rendering: the tool's `old`/`new` strings become a
// before→after diff (removed lines red, added lines green). Deterministic and
// dependency-free — no diff library. Empty inputs yield no lines.
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

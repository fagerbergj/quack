// A trailing unclosed ```mermaid fence may still be streaming, so it's a growing prefix, not a diagram.
// Mirrors internal/vetting/mermaid.go; per CommonMark only the last fence can be open.
const fenceOpenRe = /^ {0,3}(`{3,}|~{3,})[ \t]*(\S*)\s*$/
const fenceCloseRe = /^ {0,3}(`{3,}|~{3,})\s*$/

// fenceStateAtEnd is the shared walker: which fence (if any) is still open
// once `text` has been fully scanned.
function fenceStateAtEnd(text: string): { char: string; len: number; info: string } | null {
  let open: { char: string; len: number; info: string } | null = null
  for (const line of text.split('\n')) {
    if (open) {
      const close = fenceCloseRe.exec(line)
      if (close && close[1][0] === open.char && close[1].length >= open.len) open = null
      continue
    }
    const start = fenceOpenRe.exec(line)
    if (start) open = { char: start[1][0], len: start[1].length, info: (start[2] || '').toLowerCase() }
  }
  return open
}

export function isTrailingMermaidFenceOpen(text: string): boolean {
  return fenceStateAtEnd(text)?.info === 'mermaid'
}

// Only a blank line outside any open fence and not followed by indentation (a lazy continuation, as in a
// loose list) can split streaming markdown into a settled prefix and live tail. Returns -1 when none exists.
export function lastSafeSplitOffset(text: string, maxOffset: number): number {
  let idx = text.lastIndexOf('\n\n', Math.min(maxOffset, text.length))
  while (idx > 0) {
    const continuesIndented = /^[ \t]/.test(text.slice(idx + 2, idx + 3))
    if (!continuesIndented && !fenceStateAtEnd(text.slice(0, idx))) return idx
    idx = text.lastIndexOf('\n\n', idx - 1)
  }
  return -1
}

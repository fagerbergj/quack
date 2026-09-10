// Streaming heuristic: a ```mermaid fence at the very end of a message may
// still be arriving token-by-token, so its content is a growing prefix, not
// a diagram. Walk fence open/close state line-by-line (mirrors the backend's internal/vetting/mermaid.go walker) and report whether the LAST fence is an unclosed mermaid block - the only fence a streaming message can ever leave open (CommonMark: an unterminated fence swallows everything after it, so at most one can be open, and it's always last).
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

// lastSafeSplitOffset finds the last blank-line block boundary at or before
// `maxOffset` that does not fall inside an open fence of ANY language, and
// isn't immediately followed by an indented line - the only points a streaming markdown document can be cut into a settled, memoizable prefix and a short live tail without breaking an open fence, list item, footnote definition, or indented code block (AssistantText). A blank line followed by indentation is a lazy continuation, not a real boundary ("- item\n\n  more\n\n- item2" is ONE loose list); falls back to an earlier boundary when the nearest is unsafe; returns -1 when none exists.
export function lastSafeSplitOffset(text: string, maxOffset: number): number {
  let idx = text.lastIndexOf('\n\n', Math.min(maxOffset, text.length))
  while (idx > 0) {
    const continuesIndented = /^[ \t]/.test(text.slice(idx + 2, idx + 3))
    if (!continuesIndented && !fenceStateAtEnd(text.slice(0, idx))) return idx
    idx = text.lastIndexOf('\n\n', idx - 1)
  }
  return -1
}

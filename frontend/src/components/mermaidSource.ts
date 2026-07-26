// Streaming heuristic: a ```mermaid fence at the very end of a message may
// still be arriving token-by-token, so its content is a growing prefix, not
// a diagram. Walk fence open/close state line-by-line (mirrors the backend's
// internal/vetting/mermaid.go walker) and report whether the LAST fence in
// the text is an unclosed mermaid block - that's the only fence a streaming
// message can ever leave open (CommonMark: an unterminated fence swallows
// everything after it, so at most one can be open, and it's always last).
const fenceOpenRe = /^ {0,3}(`{3,}|~{3,})[ \t]*(\S*)\s*$/
const fenceCloseRe = /^ {0,3}(`{3,}|~{3,})\s*$/

export function isTrailingMermaidFenceOpen(text: string): boolean {
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
  return open?.info === 'mermaid'
}

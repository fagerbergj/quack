// A lone punctuation backtick becomes a CommonMark opener with no closer and steals the pairing of later code spans.
// fixParagraph escapes only punctuation-like or unmatched runs, leaving well-formed spans and fences untouched.
export function escapeUnmatchedBackticks(text: string): string {
  return withNonFencedParagraphs(text, fixParagraph)
}

// Mirrors CommonMark code-span matching (spec 6.1): each run seeks the next run of equal length as its closer, and
// scanning resumes past a pair. A lone backtick with whitespace on both sides is escaped outright, never paired.
function fixParagraph(text: string): string {
  const runs: { start: number; len: number }[] = []
  const re = /`+/g
  let m: RegExpExecArray | null
  while ((m = re.exec(text))) runs.push({ start: m.index, len: m[0].length })
  if (runs.length === 0) return text

  const escape = new Set<number>()
  let i = 0
  while (i < runs.length) {
    const r = runs[i]
    if (r.len === 1 && isIsolated(text, r.start)) {
      escape.add(i)
      i++
      continue
    }
    let j = i + 1
    while (j < runs.length && runs[j].len !== r.len) j++
    if (j < runs.length) {
      i = j + 1 // paired - the content between opener and closer is inert
    } else {
      escape.add(i)
      i++
    }
  }
  if (escape.size === 0) return text

  let out = ''
  let cursor = 0
  runs.forEach((r, idx) => {
    out += text.slice(cursor, r.start)
    out += escape.has(idx) ? '\\`'.repeat(r.len) : '`'.repeat(r.len)
    cursor = r.start + r.len
  })
  out += text.slice(cursor)
  return out
}

function isIsolated(text: string, pos: number): boolean {
  const isBoundary = (c: string) => c === '' || /\s/.test(c)
  return isBoundary(pos === 0 ? '' : text[pos - 1]) && isBoundary(pos + 1 >= text.length ? '' : text[pos + 1])
}

// Applies `fn` to each blank-line-delimited chunk outside fenced code (``` or ~~~); fences stay byte-identical.
function withNonFencedParagraphs(text: string, fn: (chunk: string) => string): string {
  const lines = text.split('\n')
  const fenceStart = /^ {0,3}(`{3,}|~{3,})/
  const out: string[] = []
  let i = 0
  while (i < lines.length) {
    const fence = lines[i].match(fenceStart)
    if (fence) {
      const marker = fence[1][0]
      const len = fence[1].length
      const closeRe = new RegExp(`^ {0,3}[${marker}]{${len},}\\s*$`)
      out.push(lines[i])
      i++
      while (i < lines.length && !closeRe.test(lines[i])) { out.push(lines[i]); i++ }
      if (i < lines.length) { out.push(lines[i]); i++ } // the closing fence itself
      continue
    }
    const start = i
    while (i < lines.length && !lines[i].match(fenceStart)) i++
    const chunk = lines.slice(start, i).join('\n')
    out.push(chunk.split(/(\n[ \t]*\n)/).map((part, idx) => (idx % 2 === 0 ? fn(part) : part)).join(''))
  }
  return out.join('\n')
}

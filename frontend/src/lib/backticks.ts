// #746 item 16: a single backtick used as plain punctuation ("a bare ` here")
// defeats CommonMark's greedy backtick-run pairing - it becomes an opener
// with no closer, which then steals the OPENING half of the pairing for
// every later single-backtick code span in the same paragraph, so the
// model's actual `code` spans render scrambled or as plain text. This is a
// markdown-parsing quirk, not the CSS pipeline the issue first suspected -
// fenced blocks are unaffected because they parse on a separate block-level
// rule. fixParagraph escapes only backtick runs that read as punctuation
// (whitespace on both sides) or that CommonMark's own algorithm would
// otherwise leave unmatched, so every already-well-formed span is untouched.
export function escapeUnmatchedBackticks(text: string): string {
  return withNonFencedParagraphs(text, fixParagraph)
}

// fixParagraph walks backtick runs left to right, mirroring CommonMark's own
// matching (spec 6.1 Code spans): each unconsumed run seeks the NEXT run of
// the identical length as its closer. A found pair (and everything between
// the two runs) is already valid code-span content, so scanning resumes
// after the closer - a run inside an already-matched span is never visited
// as its own opener. A lone backtick with whitespace/boundary on both sides
// never hugs content the way a real delimiter does, so it's escaped outright
// rather than risk it pairing with (and stealing) a later, real span.
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

// withNonFencedParagraphs applies `fn` to every blank-line-delimited chunk of
// `text` OUTSIDE a fenced code block (``` or ~~~), leaving fences - and
// everything inside them - byte-for-byte untouched.
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

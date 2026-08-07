import { describe, it, expect } from 'vitest'
import { escapeUnmatchedBackticks } from './backticks'

describe('escapeUnmatchedBackticks', () => {
  it('leaves well-formed inline code untouched', () => {
    const text = 'Set `QUACK_LOG_LEVEL` to `debug`.'
    expect(escapeUnmatchedBackticks(text)).toBe(text)
  })

  it('leaves text with no backticks untouched', () => {
    expect(escapeUnmatchedBackticks('nothing to see here')).toBe('nothing to see here')
  })

  // #746 item 16 repro: a bare punctuation backtick earlier in the paragraph
  // stole the pairing CommonMark would otherwise give to the real spans -
  // `QUACK_LOG_LEVEL` rendered as plain text. Escaping the stray run restores
  // both real spans.
  it('escapes a stray punctuation backtick without touching the real spans after it', () => {
    const text = "Don't use a bare ` unless needed. Instead set `QUACK_LOG_LEVEL` to `debug`."
    const fixed = escapeUnmatchedBackticks(text)
    expect(fixed).toBe("Don't use a bare \\` unless needed. Instead set `QUACK_LOG_LEVEL` to `debug`.")
  })

  it('never touches a fenced code block, even one containing an odd number of backticks', () => {
    const text = '```\nbacktick usage: `\n```'
    expect(escapeUnmatchedBackticks(text)).toBe(text)
  })

  it('fixes prose around a fenced block while leaving the fence itself untouched', () => {
    const text = "a stray ` mark\n\n```ts\nconst x = 1 ` not code, but inside a fence\n```\n\nand a real `span` after"
    const fixed = escapeUnmatchedBackticks(text)
    expect(fixed).toContain('```ts\nconst x = 1 ` not code, but inside a fence\n```')
    expect(fixed).toContain('a stray \\` mark')
    expect(fixed).toContain('`span`')
  })

  it('leaves a double-backtick span containing a literal single backtick alone', () => {
    const text = 'Use `` ` `` to reference the character itself.'
    expect(escapeUnmatchedBackticks(text)).toBe(text)
  })

  it('handles an odd trailing run at the very end of the text', () => {
    const text = 'trailing stray `'
    expect(escapeUnmatchedBackticks(text)).toBe('trailing stray \\`')
  })
})

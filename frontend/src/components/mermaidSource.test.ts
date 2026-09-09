import { describe, it, expect } from 'vitest'
import { isTrailingMermaidFenceOpen, lastSafeSplitOffset } from './mermaidSource'

describe('isTrailingMermaidFenceOpen', () => {
  it('is false for plain text with no fence', () => {
    expect(isTrailingMermaidFenceOpen('just some prose')).toBe(false)
  })

  it('is false once the mermaid fence has closed', () => {
    const text = 'before\n\n```mermaid\nflowchart TD\n  A --> B\n```\n\nafter'
    expect(isTrailingMermaidFenceOpen(text)).toBe(false)
  })

  it('is false when the closed fence is the last thing in the message', () => {
    const text = '```mermaid\nflowchart TD\n  A-->B\n```'
    expect(isTrailingMermaidFenceOpen(text)).toBe(false)
  })

  it('is true while a mermaid fence is still open at end of text (mid-stream)', () => {
    const text = 'Here is a diagram:\n\n```mermaid\nflowchart TD\n  A --> B'
    expect(isTrailingMermaidFenceOpen(text)).toBe(true)
  })

  it('is true for just the opening fence line with no body yet', () => {
    expect(isTrailingMermaidFenceOpen('```mermaid')).toBe(true)
  })

  it('is false for a non-mermaid fence left open', () => {
    expect(isTrailingMermaidFenceOpen('```ts\nconst x = 1')).toBe(false)
  })

  it('is false once a later, unrelated fence closes the mermaid one and opens elsewhere', () => {
    const text = '```mermaid\nflowchart TD\n  A-->B\n```\n\n```ts\nconst x = 1'
    expect(isTrailingMermaidFenceOpen(text)).toBe(false)
  })
})

describe('lastSafeSplitOffset', () => {
  it('finds the blank line nearest maxOffset', () => {
    const text = 'first paragraph\n\nsecond paragraph\n\nthird paragraph'
    const idx = text.lastIndexOf('\n\n', 30)
    expect(lastSafeSplitOffset(text, 30)).toBe(idx)
    expect(text.slice(0, lastSafeSplitOffset(text, 30))).toBe('first paragraph')
  })

  it('returns -1 when there is no blank line at all', () => {
    expect(lastSafeSplitOffset('one long paragraph with no break', 20)).toBe(-1)
  })

  it('never lands inside an open fence - falls back to the boundary before it opened', () => {
    // The only "\n\n" near maxOffset is inside the still-open fence; the fix
    // must fall back to the blank line before the fence started.
    const text = 'settled prose\n\n```go\nfunc a() {}\n\nfunc b() {}\n'
    const openFenceStart = text.indexOf('```go')
    const idx = lastSafeSplitOffset(text, text.length)
    expect(idx).toBeGreaterThan(0)
    expect(idx).toBeLessThanOrEqual(openFenceStart)
    expect(text.slice(0, idx)).toBe('settled prose')
  })

  it('returns -1 when a single open fence spans from the very start', () => {
    const text = '```go\nfunc a() {}\n\nfunc b() {}\n'
    expect(lastSafeSplitOffset(text, text.length)).toBe(-1)
  })

  it('is safe once the fence has closed - the blank line after it is a valid split', () => {
    const text = 'intro\n\n```go\nfunc a() {}\n```\n\nmore prose after'
    const idx = lastSafeSplitOffset(text, text.length)
    expect(idx).toBeGreaterThan(text.indexOf('```go'))
    expect(text.slice(idx).trimStart()).toBe('more prose after')
  })
})

import { describe, it, expect } from 'vitest'
import { isTrailingMermaidFenceOpen } from './mermaidSource'

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

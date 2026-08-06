import { describe, it, expect } from 'vitest'
import { routeFor } from './router'

describe('routeFor', () => {
  it('matches /memory exactly', () => {
    expect(routeFor('/memory')).toBe('memory')
  })

  it('matches a /memory/... sub-path', () => {
    expect(routeFor('/memory/')).toBe('memory')
    expect(routeFor('/memory/repo:NightsOut')).toBe('memory')
  })

  // The failure mode of a bare startsWith('/memory') prefix match: a
  // same-prefix, different route must not resolve to Memory.
  it('does not match /memory-export or other same-prefix paths', () => {
    expect(routeFor('/memory-export')).toBe('chat')
    expect(routeFor('/memory-foo')).toBe('chat')
  })

  it('defaults everything else, including chat routes, to chat', () => {
    expect(routeFor('/')).toBe('chat')
    expect(routeFor('/chat/abc-123')).toBe('chat')
  })
})

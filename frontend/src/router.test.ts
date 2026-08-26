// @vitest-environment jsdom
import { describe, it, expect } from 'vitest'
import { navigate, routeFor } from './router'

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

  it('matches /ext/:name (#870 extension host)', () => {
    expect(routeFor('/ext/usage')).toBe('ext')
    expect(routeFor('/ext/usage/')).toBe('ext')
  })

  it('does not match /extra or other /ext-prefixed-but-different paths', () => {
    expect(routeFor('/extra')).toBe('chat')
  })

  it('defaults everything else, including chat routes, to chat', () => {
    expect(routeFor('/')).toBe('chat')
    expect(routeFor('/chat/abc-123')).toBe('chat')
  })
})

describe('navigate', () => {
  it('drops the query when the path ends in a bare "?" but keeps it otherwise', () => {
    window.history.replaceState(null, '', '/chat/a?q=x')
    navigate('/chat/a?', { replace: true })
    expect(window.location.search).toBe('')

    window.history.replaceState(null, '', '/chat/a?q=x')
    navigate('/chat/b', { replace: true })
    expect(window.location.search).toBe('?q=x')
  })
})

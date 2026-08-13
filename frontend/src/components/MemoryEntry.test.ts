// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryEntry, memoryTier, memoryTierLabel, memoryTierBadgeClass } from './MemoryEntry'
import type { Memory } from '../api'

const BASE: Memory = {
  id: 'e5f4a1',
  content: "NightsOut's instrumentation tests need minSdk 30 for DEX version 040.",
  bucket: 'repo:NightsOut',
  author: 'code-implementer',
  timestamp: '2026-08-04T18:22:11Z',
  kind: 'repo',
}

describe('memoryTier (design doc §3/§8 step 6 - lifecycle tier)', () => {
  it('reads a missing status as unverified - a memory minted before the field existed', () => {
    expect(memoryTier({})).toBe('unverified')
    expect(memoryTier({ status: undefined })).toBe('unverified')
  })

  it('passes reinforced and invalidated through unchanged', () => {
    expect(memoryTier({ status: 'reinforced' })).toBe('reinforced')
    expect(memoryTier({ status: 'invalidated' })).toBe('invalidated')
  })
})

describe('memoryTierLabel', () => {
  it('labels a reinforced memory with its reinforcement count', () => {
    expect(memoryTierLabel({ status: 'reinforced', reinforcement_count: 3 })).toBe('reinforced ×3')
  })

  it('falls back to ×0 when a reinforced memory somehow carries no count', () => {
    expect(memoryTierLabel({ status: 'reinforced' })).toBe('reinforced ×0')
  })

  it('labels unverified and invalidated with the bare tier name', () => {
    expect(memoryTierLabel({ status: undefined })).toBe('unverified')
    expect(memoryTierLabel({ status: 'invalidated' })).toBe('invalidated')
  })
})

describe('memoryTierBadgeClass', () => {
  it('maps each tier to a distinct, dark-theme-aware color', () => {
    expect(memoryTierBadgeClass('reinforced')).toContain('green')
    expect(memoryTierBadgeClass('invalidated')).toContain('red')
    expect(memoryTierBadgeClass('unverified')).toContain('gray')
    for (const tier of ['reinforced', 'invalidated', 'unverified'] as const) {
      expect(memoryTierBadgeClass(tier)).toMatch(/dark:/)
    }
  })
})

describe('MemoryEntry tier rendering', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function render(memory: Memory) {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(MemoryEntry, { memory, onForget: async () => {} }))
    })
    return host
  }

  it('shows the unverified badge for a pre-lifecycle memory with no status', () => {
    const el = render(BASE)
    expect(el.textContent).toContain('unverified')
  })

  it('shows the reinforcement count for a reinforced memory', () => {
    const el = render({ ...BASE, status: 'reinforced', reinforcement_count: 5 })
    expect(el.textContent).toContain('reinforced ×5')
  })

  it('shows the invalidation reason inline for an invalidated memory', () => {
    const el = render({ ...BASE, status: 'invalidated', invalidation_reason: 'pr closed unmerged' })
    expect(el.textContent).toContain('invalidated')
    expect(el.textContent).toContain('pr closed unmerged')
  })

  it('renders no reason line for an invalidated memory carrying no reason', () => {
    const el = render({ ...BASE, status: 'invalidated' })
    expect(el.textContent).toContain('invalidated')
  })
})

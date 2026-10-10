// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { createElement } from 'react'
import { cleanup, render } from '@testing-library/react'
import { MemoryEntry, memoryTier, memoryTierLabel, memoryTierBadgeClass } from './MemoryEntry'
import type { Memory } from '../api'

afterEach(cleanup)

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

  // Backend JSON can carry a status the generated type doesn't name yet; cast past the type to simulate it.
  it('reads a status this build does not recognize as unknown, never as unverified', () => {
    expect(memoryTier({ status: 'pending_review' as Memory['status'] })).toBe('unknown')
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

  it('renders an unrecognized status as itself, not "unverified"', () => {
    expect(memoryTierLabel({ status: 'pending_review' as Memory['status'] })).toBe('pending_review')
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

  it('renders unknown the same neutral gray as unverified - never a color implying a tier we verified', () => {
    expect(memoryTierBadgeClass('unknown')).toContain('gray')
  })
})

describe('MemoryEntry tier rendering', () => {
  function mount(memory: Memory) {
    return render(createElement(MemoryEntry, { memory, onForget: async () => {}, onVote: async () => {} })).container
  }

  it('shows the unverified badge for a pre-lifecycle memory with no status', () => {
    const el = mount(BASE)
    expect(el.textContent).toContain('unverified')
  })

  it('shows the reinforcement count for a reinforced memory', () => {
    const el = mount({ ...BASE, status: 'reinforced', reinforcement_count: 5 })
    expect(el.textContent).toContain('reinforced ×5')
  })

  it('shows the invalidation reason inline for an invalidated memory', () => {
    const el = mount({ ...BASE, status: 'invalidated', invalidation_reason: 'pr closed unmerged' })
    expect(el.textContent).toContain('invalidated')
    expect(el.textContent).toContain('pr closed unmerged')
  })

  it('renders no reason line for an invalidated memory carrying no reason', () => {
    const el = mount({ ...BASE, status: 'invalidated' })
    expect(el.textContent).toContain('invalidated')
  })

  it('renders an unrecognized status as its raw value on the lifecycle badge', () => {
    // The row may still say "unverified" via the separate vote-based tier; memoryTierLabel's unit tests
    // above cover this badge specifically.
    const el = mount({ ...BASE, status: 'pending_review' as Memory['status'] })
    expect(el.textContent).toContain('pending_review')
  })

  it('still reads a missing status as unverified (unchanged by the unknown-status handling)', () => {
    const el = mount({ ...BASE, status: undefined })
    expect(el.textContent).toContain('unverified')
  })
})

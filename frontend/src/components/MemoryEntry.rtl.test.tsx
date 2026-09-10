// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryEntry } from './MemoryEntry'
import type { Memory } from '../api'

afterEach(cleanup)

const memory: Memory = {
  id: 'm1', content: 'A fact', bucket: 'repo:quack', author: 'agent', timestamp: new Date().toISOString(), kind: 'fact',
  vote_score: 3, upvotes: 3, downvotes: 0, tier: 'verified',
}

// #1137: the Forget control (now in the kebab menu, epic #1255 P4) measured
// 28x28px (w-7 h-7) - under the 44px comfortable touch target. min-w/min-h
// fixes the hit area without growing the glyph itself.
describe('MemoryEntry kebab menu touch target (#1137)', () => {
  it('kebab trigger is a 44x44 tap area', () => {
    render(<MemoryEntry memory={memory} onForget={async () => {}} onVote={async () => {}} />)
    const btn = screen.getByRole('button', { name: 'Memory actions' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })

  it('Forget lives in the kebab menu, not a top-level action button', () => {
    render(<MemoryEntry memory={memory} onForget={async () => {}} onVote={async () => {}} />)
    expect(screen.queryByRole('button', { name: /Forget/ })).toBeNull()
  })
})

// epic #1255 P4: the vote control sends the request and updates the score
// optimistically via the caller's onVote; clicking the active arrow again
// toggles the vote off.
describe('MemoryEntry vote control', () => {
  it('clicking an arrow sends the vote', async () => {
    const onVote = vi.fn().mockResolvedValue(undefined)
    render(<MemoryEntry memory={memory} onForget={async () => {}} onVote={onVote} />)
    await userEvent.click(screen.getByRole('button', { name: 'Upvote' }))
    expect(onVote).toHaveBeenCalledWith('m1', 'up')
  })

  it('re-clicking the own-vote arrow sends none (toggle off)', async () => {
    const votedMemory: Memory = { ...memory, own_vote: 'up' }
    const onVote = vi.fn().mockResolvedValue(undefined)
    render(<MemoryEntry memory={votedMemory} onForget={async () => {}} onVote={onVote} />)
    await userEvent.click(screen.getByRole('button', { name: 'Upvote' }))
    expect(onVote).toHaveBeenCalledWith('m1', 'none')
  })
})

// #1300 review: mintedTimeRelative was memoized on [memory.timestamp] - a
// value that never changes for a given memory - so it froze at the first
// computed wall-clock time. A refetch handing the row a new Memory object (same timestamp, different reference - what a real store update looks like) passes memo(MemoryEntry)'s shallow prop check and re-renders, but the frozen memo hid the correct new label.
describe('MemoryEntry minted-time freshness (#1300 review)', () => {
  it('reflects the current time on a re-render with a new memory object, not a cached one', () => {
    vi.useFakeTimers()
    try {
      vi.setSystemTime(new Date('2026-01-01T00:00:00Z'))
      const original: Memory = { ...memory, timestamp: '2026-01-01T00:00:00Z' }
      const { rerender } = render(<MemoryEntry memory={original} onForget={async () => {}} onVote={async () => {}} />)
      expect(screen.queryByText('just now')).not.toBeNull()

      // Two hours pass; the store refetches and hands down a brand-new object
      // for the same memory (same timestamp value, new reference).
      vi.setSystemTime(new Date('2026-01-01T02:00:00Z'))
      const refetched: Memory = { ...original }
      rerender(<MemoryEntry memory={refetched} onForget={async () => {}} onVote={async () => {}} />)

      expect(screen.queryByText('just now')).toBeNull()
      expect(screen.queryByText('2h ago')).not.toBeNull()
    } finally {
      vi.useRealTimers()
    }
  })
})

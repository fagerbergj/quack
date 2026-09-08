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

// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { VoteControl } from './VoteControl'

afterEach(cleanup)

describe('VoteControl', () => {
  it('clicking up with no prior vote sends "up"', async () => {
    const onVote = vi.fn().mockResolvedValue(undefined)
    render(<VoteControl score={0} onVote={onVote} />)
    await userEvent.click(screen.getByRole('button', { name: 'Upvote' }))
    expect(onVote).toHaveBeenCalledWith('up')
  })

  it('clicking the already-active up arrow sends "none" (toggle off)', async () => {
    const onVote = vi.fn().mockResolvedValue(undefined)
    render(<VoteControl score={1} ownVote="up" onVote={onVote} />)
    await userEvent.click(screen.getByRole('button', { name: 'Upvote' }))
    expect(onVote).toHaveBeenCalledWith('none')
  })

  it('clicking down while up is active sends "down" (switch)', async () => {
    const onVote = vi.fn().mockResolvedValue(undefined)
    render(<VoteControl score={1} ownVote="up" onVote={onVote} />)
    await userEvent.click(screen.getByRole('button', { name: 'Downvote' }))
    expect(onVote).toHaveBeenCalledWith('down')
  })

  it('shows an inline error and stays clickable when onVote rejects', async () => {
    const onVote = vi.fn().mockRejectedValue(new Error('boom'))
    render(<VoteControl score={0} onVote={onVote} />)
    await userEvent.click(screen.getByRole('button', { name: 'Upvote' }))
    expect(await screen.findByText('Vote failed')).toBeTruthy()
  })

  it('both arrows are 44x44 tap targets', () => {
    render(<VoteControl score={0} onVote={async () => {}} />)
    for (const name of ['Upvote', 'Downvote']) {
      const btn = screen.getByRole('button', { name })
      expect(btn.className).toContain('min-w-[44px]')
      expect(btn.className).toContain('min-h-[44px]')
    }
  })
})

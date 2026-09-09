// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { ChatHeaderStatus } from './Chat'

afterEach(cleanup)

// Audit #6: a chat with a live run shows its state and elapsed time in the
// header, not only inside the node cards.
describe('ChatHeaderStatus', () => {
  it('names the run state and ticks an elapsed time from startedAt', () => {
    render(<ChatHeaderStatus status="running" startedAt={Date.now() - 65_000} />)
    expect(screen.getByRole('img', { name: 'Status: Running' })).toBeTruthy()
    expect(screen.getByText(/^1m \d+s$/)).toBeTruthy()
  })

  it('shows the needs-input state with no elapsed when the start is unknown', () => {
    render(<ChatHeaderStatus status="needs_input" />)
    expect(screen.getByRole('img', { name: 'Status: Needs input' })).toBeTruthy()
    expect(screen.queryByText(/\d+s$/)).toBeNull()
  })
})

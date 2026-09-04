// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ChatMenu } from './ChatMenu'

afterEach(cleanup)

// #1136: the header hides its inline token/model UsageSummary below the
// medium (600px) size class - this is the escape hatch, always available
// through the same kebab regardless of width.
describe('ChatMenu usage row', () => {
  it('shows the token/model summary in the menu when a usage prop is given', async () => {
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" usage={{ models: ['gpt-5'], usage: { total_tokens: 5522 } }} />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.getByText('gpt-5')).toBeTruthy()
    expect(screen.getByText('5,522 tok')).toBeTruthy()
  })

  it('omits the usage row when no usage prop is given', async () => {
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.queryByText(/tok$/)).toBeNull()
  })

  it('the trigger button meets the 44px touch-target floor', () => {
    render(<ChatMenu chatId="c1" />)
    const btn = screen.getByRole('button', { name: 'Chat actions' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })
})

// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NavRail } from './NavRail'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

// Compact (#1131/#1133): below 600px the persistent rail is replaced by a
// hamburger + off-canvas drawer. matchMedia is mocked to report compact -
// there's no real viewport in jsdom to trigger it.
function mockCompact(matches: boolean) {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

describe('NavRail compact drawer', () => {
  it('renders only a hamburger, no drawer, until opened', () => {
    mockCompact(true)
    render(<NavRail route="chat" initialExtensions={[]} />)
    expect(screen.getByRole('button', { name: 'Open navigation' })).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByText('Chats')).toBeNull()
  })

  it('opens the drawer on hamburger click, closes on Esc, and returns focus to the hamburger', async () => {
    mockCompact(true)
    const user = userEvent.setup()
    render(<NavRail route="chat" initialExtensions={[]} />)
    const opener = screen.getByRole('button', { name: 'Open navigation' })
    await user.click(opener)

    const dialog = await screen.findByRole('dialog', { name: 'Main navigation' })
    expect(dialog).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Chats' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Memory' })).toBeTruthy()
    // Opening moves focus into the panel - the first focusable in it is "Close navigation".
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close navigation' }))

    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(document.activeElement).toBe(opener)
  })

  it('closing the drawer via the backdrop close button also works', async () => {
    mockCompact(true)
    const user = userEvent.setup()
    render(<NavRail route="chat" initialExtensions={[]} />)
    await user.click(screen.getByRole('button', { name: 'Open navigation' }))
    await screen.findByRole('dialog')
    await user.click(screen.getByRole('button', { name: 'Close navigation' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('renders the persistent rail (no hamburger) above the compact width', () => {
    mockCompact(false)
    render(<NavRail route="chat" initialExtensions={[]} />)
    expect(screen.queryByRole('button', { name: 'Open navigation' })).toBeNull()
    expect(screen.getByText('Chats')).toBeTruthy()
  })
})

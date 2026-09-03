// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ChatList } from './ChatList'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  window.history.replaceState(null, '', '/')
})

function mockCompact(matches: boolean) {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

function baseProps(onCloseMobile: () => void) {
  return {
    chats: [],
    activeChatId: null,
    onSelect: () => {},
    onNewChat: () => {},
    onDelete: () => {},
    onCloseMobile,
  }
}

// #1131: the chat list's existing mobile drawer picks up the same a11y
// wiring (Esc closes, focus returns) NavRail's new drawer uses.
describe('ChatList mobile drawer a11y', () => {
  it('Esc closes the open drawer and returns focus to the trigger', async () => {
    mockCompact(true)
    const onCloseMobile = vi.fn()
    const user = userEvent.setup()
    const trigger = document.createElement('button')
    trigger.textContent = 'open'
    document.body.appendChild(trigger)
    trigger.focus()

    render(<ChatList {...baseProps(onCloseMobile)} open={true} />)
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'Chat list' })).toBeTruthy())

    await user.keyboard('{Escape}')
    expect(onCloseMobile).toHaveBeenCalled()
    trigger.remove()
  })

  it('is not a dialog when closed or above the compact width', () => {
    mockCompact(true)
    render(<ChatList {...baseProps(() => {})} open={false} />)
    expect(screen.queryByRole('dialog')).toBeNull()

    cleanup()
    mockCompact(false)
    render(<ChatList {...baseProps(() => {})} open={true} />)
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

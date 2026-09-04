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
  it('opening moves focus into the panel, Esc closes it, and closing returns focus to the trigger', async () => {
    mockCompact(true)
    const onCloseMobile = vi.fn()
    const user = userEvent.setup()
    const trigger = document.createElement('button')
    trigger.textContent = 'open'
    document.body.appendChild(trigger)
    trigger.focus()

    const { rerender } = render(<ChatList {...baseProps(onCloseMobile)} open={true} />)
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'Chat list' })).toBeTruthy())
    // Opening moves focus into the panel - the first focusable in it is "New Chat".
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'New Chat' }))

    await user.keyboard('{Escape}')
    expect(onCloseMobile).toHaveBeenCalled()

    // onCloseMobile is a spy here (doesn't flip real state) - drive the actual
    // close the caller would perform, so the effect's cleanup (focus-restore) runs.
    rerender(<ChatList {...baseProps(onCloseMobile)} open={false} />)
    expect(document.activeElement).toBe(trigger)
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

// #1201: "Load more" measured 249x36px on mobile - under the 44px comfortable
// touch target. min-h-[44px] fixes the height without visual bloat (the text
// stays the same size, only the button's padding grows).
describe('ChatList "Load more" touch target (#1201)', () => {
  it('active-list Load more has a >=44px min-height', () => {
    render(<ChatList {...baseProps(() => {})} open={false} hasMoreChats onLoadMoreChats={() => {}} />)
    const btn = screen.getByText('Load more').closest('button')!
    expect(btn.className).toContain('min-h-[44px]')
  })
})

// #1137: the per-row "Archive chat" (×) button was packed tightly against the
// row's own "Row actions" (⋮) menu trigger on archived rows - both are now
// full 44x44 tap areas placed side by side instead of overlapping/adjacent
// small boxes.
describe('ChatList archive/row-actions touch targets (#1137)', () => {
  const chat = {
    id: 'c1', title: 'A chat', system_prompt: '', created_at: '', updated_at: '', status: 'idle',
  } as const

  it('the archive/delete button is a 44x44 tap area', () => {
    render(<ChatList {...baseProps(() => {})} open={false} chats={[chat]} />)
    const btn = screen.getByRole('button', { name: 'Archive chat' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })

  it('an archived row\'s row-actions menu trigger is also a 44x44 tap area', async () => {
    const user = userEvent.setup()
    render(<ChatList {...baseProps(() => {})} open={false} archivedChats={[chat]} onUnarchive={() => {}} />)
    await user.click(screen.getByRole('button', { name: /Archived/ }))
    const btn = screen.getByRole('button', { name: 'Row actions' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })
})

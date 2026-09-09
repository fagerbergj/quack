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

// #1137/#1319: every row's kebab (the row's only action point) is a 44x44
// tap area, on both active and archived rows.
describe('ChatList kebab touch target (#1137)', () => {
  const chat = {
    id: 'c1', title: 'A chat', system_prompt: '', created_at: '', updated_at: '', status: 'idle',
  } as const

  it('an active row\'s kebab is a 44x44 tap area', () => {
    render(<ChatList {...baseProps(() => {})} open={false} chats={[chat]} />)
    const btn = screen.getByRole('button', { name: 'Chat actions' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })

  it('an archived row\'s kebab is also a 44x44 tap area', async () => {
    const user = userEvent.setup()
    render(<ChatList {...baseProps(() => {})} open={false} archivedChats={[chat]} onUnarchive={() => {}} />)
    await user.click(screen.getByRole('button', { name: /Archived/ }))
    const btn = screen.getByRole('button', { name: 'Chat actions' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })
})

// #1319: owner instruction - archive and delete both live behind the same
// always-visible kebab (two clicks), never a bare one-tap control.
describe('ChatList row kebab (#1319)', () => {
  const activeChat = {
    id: 'c1', title: 'Active chat', system_prompt: '', created_at: '', updated_at: '', status: 'idle',
  } as const
  const archivedChat = {
    id: 'c2', title: 'Archived chat', system_prompt: '', created_at: '', updated_at: '', status: 'idle', archived: true,
  } as const

  it('active row: kebab is visible, Archive is hidden until opened, and choosing it calls onArchive', async () => {
    const user = userEvent.setup()
    const onArchive = vi.fn()
    render(<ChatList {...baseProps(() => {})} open={false} chats={[activeChat]} onArchive={onArchive} />)

    expect(screen.getByRole('button', { name: 'Chat actions' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Archive chat' })).toBeNull()
    expect(screen.queryByRole('menuitem', { name: 'Archive chat' })).toBeNull()

    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    const archiveItem = screen.getByRole('menuitem', { name: 'Archive chat' })
    expect(archiveItem).toBeTruthy()

    await user.click(archiveItem)
    expect(onArchive).toHaveBeenCalledWith('c1')
  })

  it('archived row: kebab menu shows Restore and Delete, and Delete asks for confirmation', async () => {
    const user = userEvent.setup()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    const onUnarchive = vi.fn()
    const onDelete = vi.fn()
    render(<ChatList {...baseProps(() => {})} open={false} archivedChats={[archivedChat]} onUnarchive={onUnarchive} onDelete={onDelete} />)
    await user.click(screen.getByRole('button', { name: /Archived/ }))

    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.getByRole('menuitem', { name: 'Unarchive chat' })).toBeTruthy()
    expect(screen.getByRole('menuitem', { name: 'Delete chat permanently' })).toBeTruthy()

    await user.click(screen.getByRole('menuitem', { name: 'Delete chat permanently' }))
    expect(confirmSpy).toHaveBeenCalledOnce()
    expect(onDelete).toHaveBeenCalledWith('c2', expect.anything())

    confirmSpy.mockRestore()
  })

  it('Escape closes the menu and returns focus to the kebab', async () => {
    const user = userEvent.setup()
    render(<ChatList {...baseProps(() => {})} open={false} chats={[activeChat]} />)

    const kebab = screen.getByRole('button', { name: 'Chat actions' })
    await user.click(kebab)
    expect(screen.getByRole('menu')).toBeTruthy()

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(kebab)
  })
})

// The kebab's items are the only way to archive/restore/delete on a phone,
// so each is a 44px row at compact width (desktop keeps the dense menu).
describe('ChatList row kebab items are 44px at compact width', () => {
  const activeChat = {
    id: 'c1', title: 'Active chat', system_prompt: '', created_at: '', updated_at: '', status: 'idle',
  } as const
  const archivedChat = {
    id: 'c2', title: 'Archived chat', system_prompt: '', created_at: '', updated_at: '', status: 'idle', archived: true,
  } as const

  it('active row: Archive item', async () => {
    const user = userEvent.setup()
    render(<ChatList {...baseProps(() => {})} open={false} chats={[activeChat]} onArchive={() => {}} />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.getByRole('menuitem', { name: 'Archive chat' }).className).toContain('min-h-[44px]')
  })

  it('archived row: Restore and Delete items', async () => {
    const user = userEvent.setup()
    render(<ChatList {...baseProps(() => {})} open={false} archivedChats={[archivedChat]} onUnarchive={() => {}} onDelete={() => {}} />)
    await user.click(screen.getByRole('button', { name: /Archived/ }))
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    for (const name of ['Unarchive chat', 'Delete chat permanently']) {
      expect(screen.getByRole('menuitem', { name }).className).toContain('min-h-[44px]')
    }
  })
})

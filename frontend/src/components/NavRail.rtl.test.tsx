// @vitest-environment jsdom
import { useState } from 'react'
import { describe, it, expect, afterEach, beforeEach } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NavRail } from './NavRail'
import { NavToggle } from './NavToggle'
import type { ExtensionInfo } from '../api'

afterEach(() => {
  cleanup()
})

beforeEach(() => {
  window.history.replaceState(null, '', '/')
})

// #1171: NavRail is a pure drawer at every width - no persistent rail, no
// hamburger column, no compact/media-query branch, so there is nothing to
// mock the viewport for. This harness stands in for App.tsx: it owns the
// open state (always starting closed, remembering nothing) and carries the
// toggle the drawer's useDrawer focus-return targets.
function Harness({ route = 'chat', initialExtensions = [] }: { route?: 'chat' | 'memory' | 'ext'; initialExtensions?: ExtensionInfo[] }) {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <NavToggle open={open} onToggle={() => setOpen(o => !o)} />
      <NavRail route={route} open={open} onClose={() => setOpen(false)} initialExtensions={initialExtensions} />
    </div>
  )
}

describe('NavRail drawer', () => {
  it('renders nothing while closed - no dialog, no items, no rail - at every width', () => {
    render(<Harness />)
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByText('Chats')).toBeNull()
    expect(screen.queryByText('Memory')).toBeNull()
    // The only DOM the component contributes is the app's own toggle button.
    expect(document.body.querySelectorAll('button')).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'Toggle navigation' })).toBeTruthy()
  })

  it('opens the drawer with the Chats/Memory list on toggle, focus moving into the panel', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))

    const dialog = await screen.findByRole('dialog', { name: 'Main navigation' })
    expect(dialog).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Chats' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Memory' })).toBeTruthy()
    // Opening moves focus into the panel - the first focusable in it is "Close navigation".
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close navigation' }))
  })

  it('closes on Esc and returns focus to the trigger', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    const trigger = screen.getByRole('button', { name: 'Toggle navigation' })
    await user.click(trigger)
    await screen.findByRole('dialog', { name: 'Main navigation' })

    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(document.activeElement).toBe(trigger)
  })

  it('closes on the ✕ close button', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })
    await user.click(screen.getByRole('button', { name: 'Close navigation' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('closes on a backdrop tap', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })
    const backdrop = document.querySelector('.bg-black\\/50')
    expect(backdrop).not.toBeNull()
    await user.click(backdrop!)
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('closes on item selection and navigates to the picked route', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })

    await user.click(screen.getByRole('button', { name: 'Memory' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(window.location.pathname).toBe('/memory')
  })

  it('closes on extension selection and navigates to its /ext/:name route', async () => {
    const user = userEvent.setup()
    render(<Harness initialExtensions={[{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }]} />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })

    await user.click(screen.getByRole('button', { name: 'reMarkable' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(window.location.pathname).toBe('/ext/remarkable')
  })
})

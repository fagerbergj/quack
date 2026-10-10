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

// Stands in for App.tsx: owns the open state and carries the toggle that focus returns to.
// The drawer has no viewport branch, so nothing mocks the viewport.
function Harness({ route = 'chat', initialExtensions = [], versionOverride }: { route?: 'chat' | 'memory' | 'ext'; initialExtensions?: ExtensionInfo[]; versionOverride?: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <NavToggle open={open} onToggle={() => setOpen(o => !o)} />
      <NavRail route={route} open={open} onClose={() => setOpen(false)} initialExtensions={initialExtensions} versionOverride={versionOverride} />
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

  it('opens the drawer with the Chats/Memory list on toggle', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))

    const dialog = await screen.findByRole('dialog', { name: 'Main navigation' })
    expect(dialog).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Chats' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Memory' })).toBeTruthy()
  })

  it('returns focus to the trigger on close', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    const trigger = screen.getByRole('button', { name: 'Toggle navigation' })
    await user.click(trigger)
    await screen.findByRole('dialog', { name: 'Main navigation' })

    await user.click(screen.getByRole('button', { name: 'Close navigation' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(document.activeElement).toBe(trigger)
  })

  it('closes on a backdrop tap', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    // A click targeting the <dialog> itself is a ::backdrop click.
    await user.click(await screen.findByRole('dialog', { name: 'Main navigation' }))
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

  // The version footer is titled with the full string and is not one of the nav buttons.
  it('shows the version footer, muted and titled with the full string', async () => {
    const user = userEvent.setup()
    render(<Harness versionOverride="0.51.26" />)
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })

    const version = screen.getByTitle('0.51.26')
    expect(version.textContent).toBe('v0.51.26')
    expect(version.tagName).toBe('SPAN')
  })
})
